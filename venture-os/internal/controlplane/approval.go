package controlplane

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	approvalv1 "github.com/mrbiz/venture-os/gen/venture/approval/v1"
	"github.com/mrbiz/venture-os/internal/auth"
	"github.com/mrbiz/venture-os/internal/financeengine"
	"github.com/mrbiz/venture-os/internal/store"
)

type approvalSvc struct{ *Server }

func summary(a store.Approval) *approvalv1.ApprovalSummary {
	out := &approvalv1.ApprovalSummary{
		TenantId: a.TenantID.String(), BusinessName: a.BusinessName, ApprovalId: a.ID.String(), Kind: a.Kind, Status: a.Status,
		Title: a.Title, Summary: a.Summary, RequestedBy: a.RequestedBy, RiskLevel: a.Risk, Currency: a.Currency,
		CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
	}
	if a.AmountMinor != nil {
		out.AmountMinor = *a.AmountMinor
	}
	return out
}

// ListApprovals is the Global Approval Inbox: one feed across every business the caller can approve for.
func (s *approvalSvc) ListApprovals(ctx context.Context, req *connect.Request[approvalv1.ListApprovalsRequest]) (*connect.Response[approvalv1.ListApprovalsResponse], error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	status := req.Msg.Status
	if status == "" {
		status = "PENDING_APPROVAL"
	}
	switch status {
	case "PENDING_APPROVAL", "APPROVED", "REJECTED", "EXECUTING", "EXECUTED", "FAILED", "EXPIRED", "CANCELLED":
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown status"))
	}
	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	ms, err := s.Repo.Memberships(ctx, p.Sub)
	if err != nil {
		return nil, s.internal("approvals: memberships", err)
	}
	var tenants []uuid.UUID
	only := uuid.Nil
	if req.Msg.TenantId != "" {
		if only, err = parseID("tenant_id", req.Msg.TenantId); err != nil {
			return nil, err
		}
	}
	for _, m := range ms {
		if parseRole(m.Role) < roleApprover || (only != uuid.Nil && m.Tenant.ID != only) {
			continue
		}
		tenants = append(tenants, m.Tenant.ID)
	}
	if only != uuid.Nil && len(tenants) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("business not found"))
	}
	list, err := s.Repo.ListApprovals(ctx, tenants, status, limit)
	if err != nil {
		return nil, s.internal("approvals: list", err)
	}
	out := &approvalv1.ListApprovalsResponse{}
	for _, a := range list {
		out.Approvals = append(out.Approvals, summary(a))
	}
	return connect.NewResponse(out), nil
}

func (s *approvalSvc) GetApproval(ctx context.Context, req *connect.Request[approvalv1.GetApprovalRequest]) (*connect.Response[approvalv1.GetApprovalResponse], error) {
	_, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleApprover)
	if err != nil {
		return nil, err
	}
	id, err := parseID("approval_id", req.Msg.ApprovalId)
	if err != nil {
		return nil, err
	}
	a, err := s.Repo.Approval(ctx, tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("approval not found"))
	}
	if err != nil {
		return nil, s.internal("approvals: get", err)
	}
	return connect.NewResponse(&approvalv1.GetApprovalResponse{
		Approval: summary(a), PayloadJson: a.Payload, PayloadHash: store.HashHex(a.PayloadHash),
		DecidedBy: a.DecidedBy, DecisionNote: a.DecisionNote, ExecutionResultJson: a.ExecutionResult,
	}), nil
}

// DecideApproval approves or rejects. Approving binds the decision to the exact payload hash the caller saw, then
// executes the request at most once.
func (s *approvalSvc) DecideApproval(ctx context.Context, req *connect.Request[approvalv1.DecideApprovalRequest]) (*connect.Response[approvalv1.DecideApprovalResponse], error) {
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleApprover)
	if err != nil {
		return nil, err
	}
	id, err := parseID("approval_id", req.Msg.ApprovalId)
	if err != nil {
		return nil, err
	}
	if err := checkText("note", req.Msg.Note, 2000, false); err != nil {
		return nil, err
	}
	var want [32]byte
	if req.Msg.Approve {
		raw, derr := hex.DecodeString(strings.TrimSpace(req.Msg.PayloadHash))
		if derr != nil || len(raw) != 32 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("payload_hash (64 hex chars) is required to approve"))
		}
		copy(want[:], raw)
	}
	a, err := s.Repo.DecideApproval(ctx, tenant, id, req.Msg.Approve, p.Sub, req.Msg.Note, want)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("approval not found"))
	case errors.Is(err, store.ErrAlreadyDecided):
		// Approving again an approval whose execution never completed finishes the job instead of failing: the decision
		// was already made on this exact payload, so only the side effect is outstanding.
		if resp, ok := s.retryExecution(ctx, p.Sub, a, req.Msg.Approve, want); ok {
			return connect.NewResponse(resp), nil
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("this request is already %s", strings.ToLower(strings.ReplaceAll(a.Status, "_", " "))))
	case errors.Is(err, store.ErrHashMismatch):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the request changed since you reviewed it; reload and review again"))
	case errors.Is(err, store.ErrSpendCap):
		var sc *store.SpendCapError
		errors.As(err, &sc)
		s.auditUser(ctx, tenant, p.Sub, "spend_cap.blocked", "approval", id.String(), map[string]any{"dimension": sc.Dimension, "currency": sc.Currency})
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(spendCapMessage(sc)))
	case err != nil:
		return nil, s.internal("approvals: decide", err)
	}
	// The decision's audit row was written in the same transaction as the decision itself (store.DecideApproval).

	resp := &approvalv1.DecideApprovalResponse{Status: a.Status}
	if !req.Msg.Approve {
		return connect.NewResponse(resp), nil
	}
	out := s.executeDetached(ctx, a)
	resp.Status, resp.Executed, resp.ExecutionResultJson, resp.ExecutionError = out.status, out.executed, out.result, out.errMsg
	return connect.NewResponse(resp), nil
}

// executionBudget bounds one execution. It is well inside store.ExecutionLease.
const executionBudget = 2 * time.Minute

// executeDetached runs execute without the request's cancellation. The decision is already committed, so a client that
// disconnects (a closed tab, a proxy timeout) must not abandon the side effect half-way and leave the approval stuck.
func (s *Server) executeDetached(ctx context.Context, a store.Approval) execOutcome {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), executionBudget)
	defer cancel()
	return s.execute(ctx, a)
}

// retryExecution handles a repeated "approve" for an approval that was decided but whose execution is APPROVED (never
// started), FAILED, or EXECUTING under an expired lease. It re-runs only when the caller presents the stored payload
// hash, and only for kinds that have an executor. It reports ok=false when the repeat is not such a retry.
func (s *Server) retryExecution(ctx context.Context, by string, a store.Approval, approve bool, want [32]byte) (*approvalv1.DecideApprovalResponse, bool) {
	if !approve || a.PayloadHash != want {
		return nil, false
	}
	switch a.Status {
	case "APPROVED", "FAILED", "EXECUTING":
	default:
		return nil, false
	}
	if _, ok := executors[a.Kind]; !ok {
		return nil, false
	}
	out := s.executeDetached(ctx, a)
	if out.status == "EXECUTING" && !out.executed && out.errMsg == "" {
		return nil, false // someone else holds a live claim: this repeat is a plain duplicate, not a retry
	}
	s.auditUser(ctx, a.TenantID, by, "approval.execute_retry", "approval", a.ID.String(), map[string]any{"kind": a.Kind, "from": a.Status, "to": out.status})
	return &approvalv1.DecideApprovalResponse{Status: out.status, Executed: out.executed, ExecutionResultJson: out.result, ExecutionError: out.errMsg}, true
}

// ───────────────────────────── execution ─────────────────────────────

type execOutcome struct {
	status   string
	executed bool
	result   json.RawMessage
	errMsg   string
}

// executor performs the side effect of an approved request. Executors must be idempotent: the idempotency keys
// they derive from the approval id make a re-run after a crash safe.
type executor func(ctx context.Context, s *Server, a store.Approval) (json.RawMessage, error)

var executors = map[string]executor{
	"INVOICE":        executeInvoice,
	"PURCHASE_ORDER": executePurchaseOrder,
	"QUOTE":          executeQuote,
	"MEETING":        executeMeeting,
}

// execute runs the executor for an approved request. Kinds with no executor stay APPROVED: a human completes them
// outside the system (for example a payment), and nothing here pretends otherwise.
func (s *Server) execute(ctx context.Context, a store.Approval) execOutcome {
	exec, ok := executors[a.Kind]
	if !ok {
		return execOutcome{status: a.Status}
	}
	won, err := s.Repo.ClaimExecution(ctx, a.TenantID, a.ID)
	if err != nil {
		s.log().Error("claim execution", "approval", a.ID, "err", err)
		return execOutcome{status: a.Status, errMsg: "could not start execution"}
	}
	if !won {
		return execOutcome{status: "EXECUTING"} // another request already holds the claim
	}
	result, execErr := exec(ctx, s, a)
	if execErr != nil {
		s.log().Error("execution failed", "approval", a.ID, "kind", a.Kind, "err", execErr)
		fail, _ := json.Marshal(map[string]string{"error": execErr.Error()})
		if err := s.Repo.FinishExecution(ctx, a.TenantID, a.ID, false, fail); err != nil {
			s.log().Error("record failed execution", "approval", a.ID, "err", err)
		}
		return execOutcome{status: "FAILED", result: fail, errMsg: execErr.Error()}
	}
	if err := s.Repo.FinishExecution(ctx, a.TenantID, a.ID, true, result); err != nil {
		s.log().Error("record execution", "approval", a.ID, "err", err)
		return execOutcome{status: "EXECUTING", result: result, errMsg: "executed but the outcome could not be recorded; approve again in a few minutes to finish recording it (repeating it is safe)"}
	}
	return execOutcome{status: "EXECUTED", executed: true, result: result}
}

type docPayload struct {
	Counterparty string               `json:"counterparty"`
	Currency     string               `json:"currency"`
	DueInDays    int                  `json:"due_in_days"`
	Totals       financeengine.Totals `json:"totals"`
}

// verifiedDoc decodes a document payload and re-derives its totals from the lines: a payload that does not add up
// is refused even though its hash matched.
func verifiedDoc(a store.Approval) (docPayload, error) {
	var d docPayload
	if err := json.Unmarshal(a.Payload, &d); err != nil {
		return d, fmt.Errorf("payload is not a document: %w", err)
	}
	lines := make([]financeengine.Line, len(d.Totals.Lines))
	for i, l := range d.Totals.Lines {
		lines[i] = l.Line
	}
	again, err := financeengine.DocumentTotals(lines, d.Totals.TaxRateBps, d.Totals.PricesIncTax)
	if err != nil {
		return d, err
	}
	if again.TotalMinor != d.Totals.TotalMinor || again.TaxMinor != d.Totals.TaxMinor || again.SubtotalMinor != d.Totals.SubtotalMinor {
		return d, errors.New("document totals do not match its lines")
	}
	if a.AmountMinor == nil || *a.AmountMinor != d.Totals.TotalMinor {
		return d, errors.New("approval amount does not match the document total")
	}
	return d, nil
}

func executeInvoice(ctx context.Context, s *Server, a store.Approval) (json.RawMessage, error) {
	d, err := verifiedDoc(a)
	if err != nil {
		return nil, err
	}
	bp, err := s.Repo.ApprovedBlueprint(ctx, a.TenantID)
	if err != nil {
		return nil, fmt.Errorf("load chart of accounts: %w", err)
	}
	sys := bp.Blueprint.Accounting.SystemAccounts
	entries := []store.LedgerEntry{
		{AccountCode: sys.Receivable, Debit: d.Totals.TotalMinor, Currency: d.Currency},
		{AccountCode: sys.Revenue, Credit: d.Totals.SubtotalMinor, Currency: d.Currency},
	}
	if d.Totals.TaxMinor > 0 {
		entries = append(entries, store.LedgerEntry{AccountCode: sys.TaxPayable, Credit: d.Totals.TaxMinor, Currency: d.Currency})
	}
	journal, createdJ, err := s.Repo.PostJournal(ctx, a.TenantID, store.Journal{
		Memo: "Invoice to " + d.Counterparty, SourceType: "approval", SourceID: a.ID.String(),
		CreatedBy: a.DecidedBy, IdempotencyKey: "approval:" + a.ID.String() + ":journal", Entries: entries,
	})
	if err != nil {
		return nil, fmt.Errorf("post journal: %w", err)
	}
	due := s.now().AddDate(0, 0, d.DueInDays)
	createdC, err := s.Repo.AddCashflowItem(ctx, a.TenantID, store.CashflowItem{
		Direction: "INFLOW", Category: "invoice", AmountMinor: d.Totals.TotalMinor, Currency: d.Currency, Due: due,
		Status: "COMMITTED", SourceType: "approval", SourceID: a.ID.String(), Memo: "Invoice to " + d.Counterparty,
		IdempotencyKey: "approval:" + a.ID.String() + ":cashflow",
	})
	if err != nil {
		return nil, fmt.Errorf("schedule receipt: %w", err)
	}
	return json.Marshal(map[string]any{"journal_id": journal, "journal_created": createdJ, "receipt_scheduled": createdC, "due": due.Format("2006-01-02")})
}

func executePurchaseOrder(ctx context.Context, s *Server, a store.Approval) (json.RawMessage, error) {
	d, err := verifiedDoc(a)
	if err != nil {
		return nil, err
	}
	due := s.now().AddDate(0, 0, d.DueInDays)
	created, err := s.Repo.AddCashflowItem(ctx, a.TenantID, store.CashflowItem{
		Direction: "OUTFLOW", Category: "purchase_order", AmountMinor: d.Totals.TotalMinor, Currency: d.Currency, Due: due,
		Status: "COMMITTED", SourceType: "approval", SourceID: a.ID.String(), Memo: "PO to " + d.Counterparty,
		IdempotencyKey: "approval:" + a.ID.String() + ":cashflow",
	})
	if err != nil {
		return nil, fmt.Errorf("schedule payment: %w", err)
	}
	return json.Marshal(map[string]any{"payment_scheduled": created, "due": due.Format("2006-01-02")})
}

// executeQuote records that the founder released the quote. Delivering it to the customer needs the support
// channel connector, which does not exist yet, so the result says exactly that.
func executeQuote(_ context.Context, _ *Server, _ store.Approval) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"released": true, "delivered": false, "note": "approved for release; no delivery channel is connected"})
}

// spendCapMessage says, in money, which ceiling an approval would break and what to do about it.
func spendCapMessage(e *store.SpendCapError) string {
	if e.Dimension == store.DimensionNoCap {
		return fmt.Sprintf("Blocked by the spend cap: this business has spend caps but none for %s, so a %s payment cannot be checked against a ceiling. Nothing was approved. A founder can set a %s cap first (a high one if it should be effectively unlimited).",
			e.Currency, financeengine.FormatMinor(e.Requested, e.Currency), e.Currency)
	}
	if e.Dimension == "per transaction" {
		return fmt.Sprintf("Blocked by the spend cap: %s is more than the per-transaction cap of %s. Nothing was approved. A founder can raise the cap first.",
			financeengine.FormatMinor(e.Requested, e.Currency), financeengine.FormatMinor(e.Cap, e.Currency))
	}
	return fmt.Sprintf("Blocked by the spend cap: %s was already approved %s, and adding %s would pass the cap of %s. Nothing was approved. Wait for the window to pass, or a founder can raise the cap.",
		financeengine.FormatMinor(e.Spent, e.Currency), e.Dimension, financeengine.FormatMinor(e.Requested, e.Currency), financeengine.FormatMinor(e.Cap, e.Currency))
}
