package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	"github.com/mrbiz/venture-os/internal/bearer"
	"github.com/mrbiz/venture-os/internal/financeengine"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/store"
)

type agentCtxKey struct{}

// mcpInterceptor authenticates AgentService calls with an MCP bearer token. The tenant comes from the token and
// from nowhere else.
func mcpInterceptor(signer *mcptoken.Signer) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			token, ok := bearer.Token(req.Header().Get("Authorization"))
			if !ok {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
			}
			claims, err := signer.Verify(token)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
			}
			return next(context.WithValue(ctx, agentCtxKey{}, claims), req)
		}
	})
}

type agentSvc struct{ *Server }

// session resolves the calling agent's tenant from its token and requires the business to be live.
func (s *agentSvc) session(ctx context.Context) (mcptoken.Claims, store.Tenant, store.StoredBlueprint, error) {
	claims, ok := ctx.Value(agentCtxKey{}).(mcptoken.Claims)
	if !ok {
		return claims, store.Tenant{}, store.StoredBlueprint{}, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	t, err := s.Repo.Tenant(ctx, claims.TenantID)
	if errors.Is(err, store.ErrNotFound) {
		return claims, t, store.StoredBlueprint{}, connect.NewError(connect.CodeNotFound, errors.New("business not found"))
	}
	if err != nil {
		return claims, t, store.StoredBlueprint{}, s.internal("agent: tenant", err)
	}
	if t.Status != "ACTIVE" {
		return claims, t, store.StoredBlueprint{}, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("the business is %s, not ACTIVE; tools are unavailable until the founder finishes setup", strings.ToLower(t.Status)))
	}
	bp, err := s.Repo.ApprovedBlueprint(ctx, claims.TenantID)
	if err != nil {
		return claims, t, bp, s.internal("agent: blueprint", err)
	}
	return claims, t, bp, nil
}

func requester(c mcptoken.Claims) string {
	if c.Agent != "" {
		return "agent:" + c.Agent
	}
	return "agent:profile"
}

func (s *agentSvc) rule(ctx context.Context, tenant uuid.UUID, name string) (financeengine.Rule, error) {
	rules, err := s.Repo.PricingRules(ctx, tenant)
	if err != nil {
		return financeengine.Rule{}, s.internal("agent: rules", err)
	}
	var names []string
	for _, r := range rules {
		if r.Name == name {
			return r, nil
		}
		names = append(names, r.Name)
	}
	return financeengine.Rule{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("no active pricing rule %q (available: %s)", name, strings.Join(names, ", ")))
}

// strictJSON decodes into v rejecting unknown fields and trailing data.
func strictJSON(raw []byte, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("input is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing data")
	}
	return nil
}

func engineErr(err error) error {
	if errors.Is(err, financeengine.ErrInvalid) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func (s *agentSvc) ListPricingRules(ctx context.Context, _ *connect.Request[agentv1.ListPricingRulesRequest]) (*connect.Response[agentv1.ListPricingRulesResponse], error) {
	_, t, _, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	rules, err := s.Repo.PricingRules(ctx, t.ID)
	if err != nil {
		return nil, s.internal("agent: rules", err)
	}
	out := &agentv1.ListPricingRulesResponse{}
	for _, r := range rules {
		pr := &agentv1.PricingRule{Name: r.Name, Model: string(r.Model), Currency: r.Currency, MarginFloorBps: int32(r.MarginFloorBps), FormulaJson: r.Formula}
		if r.MarginTargetBps != nil {
			pr.MarginTargetBps = int32(*r.MarginTargetBps)
		}
		if r.ApprovalThresholdMinor != nil {
			pr.ApprovalThresholdMinor = *r.ApprovalThresholdMinor
		}
		out.Rules = append(out.Rules, pr)
	}
	return connect.NewResponse(out), nil
}

func (s *agentSvc) price(ctx context.Context, tenant uuid.UUID, ruleName string, inputJSON []byte) (financeengine.Rule, financeengine.Result, error) {
	rule, err := s.rule(ctx, tenant, ruleName)
	if err != nil {
		return rule, financeengine.Result{}, err
	}
	var in financeengine.Input
	if err := strictJSON(inputJSON, &in); err != nil {
		return rule, financeengine.Result{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("input_json: %w", err))
	}
	res, err := financeengine.Price(rule, in)
	if err != nil {
		return rule, res, engineErr(err)
	}
	return rule, res, nil
}

func (s *agentSvc) CalculatePrice(ctx context.Context, req *connect.Request[agentv1.CalculatePriceRequest]) (*connect.Response[agentv1.CalculatePriceResponse], error) {
	_, t, _, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	_, res, err := s.price(ctx, t.ID, req.Msg.RuleName, req.Msg.InputJson)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(res)
	return connect.NewResponse(&agentv1.CalculatePriceResponse{ResultJson: raw}), nil
}

func (s *agentSvc) ForecastCashFlow(ctx context.Context, req *connect.Request[agentv1.ForecastCashFlowRequest]) (*connect.Response[agentv1.ForecastCashFlowResponse], error) {
	_, t, bp, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	days := int(req.Msg.Days)
	if days == 0 {
		days = 90
	}
	now := s.now()
	opening, err := s.Repo.AccountBalance(ctx, t.ID, bp.Blueprint.Accounting.SystemAccounts.Cash)
	if err != nil {
		return nil, s.internal("agent: cash balance", err)
	}
	if days < 1 || days > financeengine.MaxForecastDays {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("days must be between 1 and %d", financeengine.MaxForecastDays))
	}
	items, err := s.Repo.CashflowItems(ctx, t.ID, now.AddDate(0, 0, days))
	if err != nil {
		return nil, s.internal("agent: cashflow items", err)
	}
	f, err := financeengine.ForecastCash(opening, items, now, days, financeengine.Options{
		IncludeForecastInflows: req.Msg.IncludeForecastInflows, BucketDays: int(req.Msg.BucketDays),
	})
	if err != nil {
		return nil, engineErr(err)
	}
	raw, _ := json.Marshal(f)
	return connect.NewResponse(&agentv1.ForecastCashFlowResponse{ForecastJson: raw, Currency: bp.Blueprint.Business.BaseCurrency}), nil
}

func checkText(field, v string, max int, required bool) error {
	v = strings.TrimSpace(v)
	if required && v == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s is required", field))
	}
	if len(v) > max {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s is longer than %d characters", field, max))
	}
	return nil
}

func riskFor(res financeengine.Result) string {
	switch {
	case res.BelowFloor:
		return "HIGH"
	case res.RequiresApproval:
		return "MEDIUM"
	}
	return "LOW"
}

func (s *agentSvc) CreateQuote(ctx context.Context, req *connect.Request[agentv1.CreateQuoteRequest]) (*connect.Response[agentv1.CreateQuoteResponse], error) {
	claims, t, _, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	for _, c := range []error{checkText("customer", m.Customer, 200, true), checkText("note", m.Note, 2000, false), checkKey("idempotency_key", m.IdempotencyKey)} {
		if c != nil {
			return nil, c
		}
	}
	valid := int(m.ValidDays)
	if valid == 0 {
		valid = 14
	}
	if valid < 1 || valid > 90 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("valid_days must be between 1 and 90"))
	}
	_, res, err := s.price(ctx, t.ID, m.RuleName, m.InputJson)
	if err != nil {
		return nil, err
	}
	var input json.RawMessage = m.InputJson
	payload, _ := json.Marshal(map[string]any{
		"type": "quote", "customer": strings.TrimSpace(m.Customer), "valid_days": valid, "note": m.Note,
		"rule": m.RuleName, "input": input, "result": res,
	})
	amount := res.PriceMinor
	a, created, err := s.Repo.CreateApproval(ctx, t.ID, store.NewApproval{
		Kind: "QUOTE", Title: fmt.Sprintf("Quote for %s", strings.TrimSpace(m.Customer)),
		Summary:         fmt.Sprintf("%s at %s margin (floor %s)%s", financeengine.FormatMinor(res.PriceMinor, res.Currency), pct(res.MarginBps), pct(res.FloorBps), reasons(res)),
		RequestedByType: "AGENT", RequestedBy: requester(claims), Risk: riskFor(res), Payload: payload,
		AmountMinor: &amount, Currency: res.Currency, IdempotencyKey: "quote:" + m.IdempotencyKey,
	})
	if err != nil {
		return nil, s.approvalErr("agent: create quote", err)
	}
	s.auditAgent(ctx, claims, t.ID, "approval.request", a)
	raw, _ := json.Marshal(res)
	return connect.NewResponse(&agentv1.CreateQuoteResponse{Document: &agentv1.Document{
		ApprovalId: a.ID.String(), Status: a.Status, ComputedJson: raw, Created: created}}), nil
}

func reasons(r financeengine.Result) string {
	if len(r.ApprovalReasons) == 0 {
		return ""
	}
	return "; flags: " + strings.Join(r.ApprovalReasons, "; ")
}

func pct(bps int) string {
	sign := ""
	if bps < 0 {
		sign, bps = "-", -bps
	}
	return fmt.Sprintf("%s%d.%02d%%", sign, bps/100, bps%100)
}

func (s *agentSvc) approvalErr(what string, err error) error {
	if errors.Is(err, store.ErrIdempotencyMismatch) {
		return connect.NewError(connect.CodeAlreadyExists, err)
	}
	return s.internal(what, err)
}

func (s *agentSvc) auditAgent(ctx context.Context, c mcptoken.Claims, tenant uuid.UUID, action string, a store.Approval) {
	if err := s.Repo.AuditActor(ctx, tenant, "AGENT", requester(c), action, "approval", a.ID.String(),
		map[string]any{"kind": a.Kind, "status": a.Status}); err != nil {
		s.log().Error("audit write failed", "action", action, "err", err)
	}
}

// draftDocument computes and queues an invoice or purchase order.
func (s *agentSvc) draftDocument(ctx context.Context, kind string, d *agentv1.DocumentDraft) (*agentv1.Document, error) {
	claims, t, bp, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("draft is required"))
	}
	for _, c := range []error{checkText("counterparty", d.Counterparty, 200, true), checkText("reference", d.Reference, 200, false), checkKey("idempotency_key", d.IdempotencyKey)} {
		if c != nil {
			return nil, c
		}
	}
	if d.DueInDays < 0 || d.DueInDays > 365 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("due_in_days must be between 0 and 365"))
	}
	var rateBps int
	found := false
	for _, r := range bp.Blueprint.Accounting.Tax.Rates {
		if r.Code == d.TaxCode {
			rateBps, found = r.RateBps, true
		}
	}
	if !found {
		var codes []string
		for _, r := range bp.Blueprint.Accounting.Tax.Rates {
			codes = append(codes, r.Code)
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown tax_code %q (configured: %s)", d.TaxCode, strings.Join(codes, ", ")))
	}
	lines := make([]financeengine.Line, len(d.Lines))
	for i, l := range d.Lines {
		lines[i] = financeengine.Line{Description: l.Description, Quantity: l.Quantity, UnitPriceMinor: l.UnitPriceMinor}
	}
	totals, err := financeengine.DocumentTotals(lines, rateBps, bp.Blueprint.Accounting.Tax.Inclusive)
	if err != nil {
		return nil, engineErr(err)
	}
	if totals.TotalMinor <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a document must have a total greater than zero"))
	}
	currency := bp.Blueprint.Business.BaseCurrency
	label, noun := "INVOICE", "Invoice"
	if kind == "PURCHASE_ORDER" {
		label, noun = "PURCHASE_ORDER", "Purchase order"
	}
	payload, _ := json.Marshal(map[string]any{
		"type": strings.ToLower(label), "counterparty": strings.TrimSpace(d.Counterparty), "reference": d.Reference,
		"tax_code": d.TaxCode, "due_in_days": d.DueInDays, "currency": currency, "totals": totals,
	})
	amount := totals.TotalMinor
	a, created, err := s.Repo.CreateApproval(ctx, t.ID, store.NewApproval{
		Kind: kind, Title: fmt.Sprintf("%s: %s", noun, strings.TrimSpace(d.Counterparty)),
		Summary:         fmt.Sprintf("%s total (%s net + %s tax at %s), due in %d days", financeengine.FormatMinor(totals.TotalMinor, currency), financeengine.FormatMinor(totals.SubtotalMinor, currency), financeengine.FormatMinor(totals.TaxMinor, currency), pct(rateBps), d.DueInDays),
		RequestedByType: "AGENT", RequestedBy: requester(claims), Risk: "MEDIUM", Payload: payload,
		AmountMinor: &amount, Currency: currency, IdempotencyKey: strings.ToLower(label) + ":" + d.IdempotencyKey,
	})
	if err != nil {
		return nil, s.approvalErr("agent: draft document", err)
	}
	s.auditAgent(ctx, claims, t.ID, "approval.request", a)
	raw, _ := json.Marshal(totals)
	return &agentv1.Document{ApprovalId: a.ID.String(), Status: a.Status, ComputedJson: raw, Created: created}, nil
}

func (s *agentSvc) DraftInvoice(ctx context.Context, req *connect.Request[agentv1.DraftInvoiceRequest]) (*connect.Response[agentv1.DraftInvoiceResponse], error) {
	doc, err := s.draftDocument(ctx, "INVOICE", req.Msg.Draft)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentv1.DraftInvoiceResponse{Document: doc}), nil
}

func (s *agentSvc) DraftPurchaseOrder(ctx context.Context, req *connect.Request[agentv1.DraftPurchaseOrderRequest]) (*connect.Response[agentv1.DraftPurchaseOrderResponse], error) {
	doc, err := s.draftDocument(ctx, "PURCHASE_ORDER", req.Msg.Draft)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentv1.DraftPurchaseOrderResponse{Document: doc}), nil
}

const maxPayloadBytes = 32 << 10

func (s *agentSvc) RequestApproval(ctx context.Context, req *connect.Request[agentv1.RequestApprovalRequest]) (*connect.Response[agentv1.RequestApprovalResponse], error) {
	claims, t, _, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	switch m.Kind {
	case "PAYMENT", "CAMPAIGN", "AGENT_SKILL", "OTHER":
	case "QUOTE", "INVOICE", "PURCHASE_ORDER":
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s requests must use the dedicated tool so the numbers come from the finance engine", m.Kind))
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("kind must be PAYMENT, CAMPAIGN, AGENT_SKILL or OTHER"))
	}
	for _, c := range []error{checkText("title", m.Title, 200, true), checkText("summary", m.Summary, 2000, false), checkKey("idempotency_key", m.IdempotencyKey)} {
		if c != nil {
			return nil, c
		}
	}
	var payload map[string]json.RawMessage
	if len(m.PayloadJson) > maxPayloadBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("payload_json exceeds %d bytes", maxPayloadBytes))
	}
	if len(bytes.TrimSpace(m.PayloadJson)) == 0 {
		m.PayloadJson = []byte("{}")
	}
	if err := json.Unmarshal(m.PayloadJson, &payload); err != nil || payload == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("payload_json must be a JSON object"))
	}
	risk := m.RiskLevel
	switch risk {
	case "":
		risk = "MEDIUM"
	case "LOW", "MEDIUM", "HIGH":
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("risk_level must be LOW, MEDIUM or HIGH"))
	}
	var amount *int64
	currency := ""
	if m.AmountMinor != 0 || m.Currency != "" {
		if m.AmountMinor < 0 || m.AmountMinor > financeengine.MaxAmountMinor || len(m.Currency) != 3 || strings.ToUpper(m.Currency) != m.Currency {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("amount_minor must be >= 0 with a 3-letter uppercase currency"))
		}
		a := m.AmountMinor
		amount, currency = &a, m.Currency
	}
	if m.Kind == "PAYMENT" {
		if amount == nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a PAYMENT request needs an amount and currency"))
		}
		risk = "HIGH" // moving money is always high risk, whatever the agent says
	}
	canonical, _ := json.Marshal(payload)
	a, created, err := s.Repo.CreateApproval(ctx, t.ID, store.NewApproval{
		Kind: m.Kind, Title: strings.TrimSpace(m.Title), Summary: strings.TrimSpace(m.Summary),
		RequestedByType: "AGENT", RequestedBy: requester(claims), Risk: risk, Payload: canonical,
		AmountMinor: amount, Currency: currency, IdempotencyKey: "request:" + m.IdempotencyKey,
	})
	if err != nil {
		return nil, s.approvalErr("agent: request approval", err)
	}
	s.auditAgent(ctx, claims, t.ID, "approval.request", a)
	return connect.NewResponse(&agentv1.RequestApprovalResponse{Approval: approvalState(a, created)}), nil
}

func (s *agentSvc) EscalateBlocker(ctx context.Context, req *connect.Request[agentv1.EscalateBlockerRequest]) (*connect.Response[agentv1.EscalateBlockerResponse], error) {
	claims, t, _, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	for _, c := range []error{checkText("title", m.Title, 200, true), checkText("details", m.Details, 4000, true), checkKey("idempotency_key", m.IdempotencyKey)} {
		if c != nil {
			return nil, c
		}
	}
	sev := m.Severity
	if sev == "" {
		sev = "MEDIUM"
	}
	if sev != "LOW" && sev != "MEDIUM" && sev != "HIGH" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("severity must be LOW, MEDIUM or HIGH"))
	}
	payload, _ := json.Marshal(map[string]any{"type": "blocker", "details": m.Details})
	a, created, err := s.Repo.CreateApproval(ctx, t.ID, store.NewApproval{
		Kind: "OTHER", Title: "Blocked: " + strings.TrimSpace(m.Title), Summary: strings.TrimSpace(m.Details),
		RequestedByType: "AGENT", RequestedBy: requester(claims), Risk: sev, Payload: payload, IdempotencyKey: "blocker:" + m.IdempotencyKey,
	})
	if err != nil {
		return nil, s.approvalErr("agent: escalate", err)
	}
	s.auditAgent(ctx, claims, t.ID, "blocker.escalate", a)
	return connect.NewResponse(&agentv1.EscalateBlockerResponse{Approval: approvalState(a, created)}), nil
}

func (s *agentSvc) GetApprovalStatus(ctx context.Context, req *connect.Request[agentv1.GetApprovalStatusRequest]) (*connect.Response[agentv1.GetApprovalStatusResponse], error) {
	_, t, _, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("approval_id", req.Msg.ApprovalId)
	if err != nil {
		return nil, err
	}
	a, err := s.Repo.Approval(ctx, t.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("approval not found"))
	}
	if err != nil {
		return nil, s.internal("agent: approval status", err)
	}
	return connect.NewResponse(&agentv1.GetApprovalStatusResponse{Approval: approvalState(a, false)}), nil
}

func approvalState(a store.Approval, created bool) *agentv1.ApprovalState {
	return &agentv1.ApprovalState{ApprovalId: a.ID.String(), Status: a.Status, DecisionNote: a.DecisionNote, Created: created}
}
