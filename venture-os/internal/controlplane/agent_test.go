package controlplane_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	approvalv1 "github.com/mrbiz/venture-os/gen/venture/approval/v1"
	"github.com/mrbiz/venture-os/gen/venture/approval/v1/approvalv1connect"
	portfoliov1 "github.com/mrbiz/venture-os/gen/venture/portfolio/v1"
	"github.com/mrbiz/venture-os/gen/venture/portfolio/v1/portfoliov1connect"
	provisionerv1 "github.com/mrbiz/venture-os/gen/venture/provisioner/v1"
	tenantv1 "github.com/mrbiz/venture-os/gen/venture/tenant/v1"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/store"
)

func (e *env) agent(token string) agentv1connect.AgentServiceClient {
	return agentv1connect.NewAgentServiceClient(http.DefaultClient, e.url, connect.WithInterceptors(bearer(token)))
}

func (e *env) approvals(token string) approvalv1connect.ApprovalServiceClient {
	return approvalv1connect.NewApprovalServiceClient(http.DefaultClient, e.url, connect.WithInterceptors(bearer(token)))
}

func bearer(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			return next(c, req)
		}
	})
}

type business struct {
	id    string
	uuid  uuid.UUID
	agent agentv1connect.AgentServiceClient
}

// goLive creates a business owned by the founder behind `oidcToken`, approves the EXIM blueprint under `slug`,
// provisions it, seeds INR 5,000,000.00 of cash, and returns an agent client holding that tenant's MCP token.
func goLive(t *testing.T, e *env, oidcToken, slug string) business {
	t.Helper()
	f := e.as(oidcToken)
	cb, err := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "biz-" + slug}))
	if err != nil {
		t.Fatal(err)
	}
	tid := mustUUID(t, cb.Msg.TenantId)
	bp := fixtures.EXIMBlueprint()
	bp.Business.Slug, bp.Business.Name = slug, slug
	sb, err := e.store.SaveBlueprint(ctx, tid, nil, bp, "seed", "bp-"+slug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ApproveBlueprint(ctx, tid, sb.ID, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.prov.Provision(ctx, connect.NewRequest(&provisionerv1.ProvisionRequest{TenantId: cb.Msg.TenantId, BlueprintId: sb.ID.String(), IdempotencyKey: "prov-" + slug})); err != nil {
		t.Fatal(err)
	}
	// Opening capital: Dr bank 5,000,000.00 / Cr owner's equity.
	_, _, err = e.store.PostJournal(ctx, tid, store.Journal{
		Memo: "opening capital", CreatedBy: "seed", IdempotencyKey: "capital", EffectiveDate: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Entries: []store.LedgerEntry{
			{AccountCode: "1010", Debit: 500000000, Currency: "INR"},
			{AccountCode: "3000", Credit: 500000000, Currency: "INR"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := e.signer.Mint(tid, "sales", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return business{id: cb.Msg.TenantId, uuid: tid, agent: e.agent(tok)}
}

const eximInputFOB = `{"exim":{"incoterm":"FOB","quantity":1000,"unit_cost_minor":800,"packing_minor":50000,"inland_freight_minor":30000}}`

func TestAgentToolsRequireAnActiveBusinessAndValidToken(t *testing.T) {
	e := newEnv(t)
	f := e.as("tok-founder")
	cb, _ := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "k"}))
	draftTok, _ := e.signer.Mint(mustUUID(t, cb.Msg.TenantId), "", time.Hour)

	// A DRAFT business has no live tools.
	_, err := e.agent(draftTok).ListPricingRules(ctx, connect.NewRequest(&agentv1.ListPricingRulesRequest{}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("draft business: %v", err)
	}
	// No token, a forged token and an OIDC user token are all refused.
	for name, tok := range map[string]string{"none": "", "forged": "abc.def.ghi", "oidc": "tok-founder"} {
		_, err := e.agent(tok).ListPricingRules(ctx, connect.NewRequest(&agentv1.ListPricingRulesRequest{}))
		if code(err) != connect.CodeUnauthenticated {
			t.Errorf("%s token: %v", name, err)
		}
	}
	// And an agent's MCP token cannot call the founder-facing APIs.
	agentOnly := e.as(draftTok)
	if _, err := agentOnly.tenant.ListBusinesses(ctx, connect.NewRequest(&tenantv1.ListBusinessesRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("MCP token on TenantService: %v", err)
	}
}

func TestPricingToolsAreDeterministicAndStrict(t *testing.T) {
	e := newEnv(t)
	b := goLive(t, e, "tok-founder", "kerala-spice-exports")

	rules, err := b.agent.ListPricingRules(ctx, connect.NewRequest(&agentv1.ListPricingRulesRequest{}))
	if err != nil || len(rules.Msg.Rules) != 1 || rules.Msg.Rules[0].Name != "eu-landed-cost" || rules.Msg.Rules[0].MarginFloorBps != 1200 {
		t.Fatalf("rules: %+v %v", rules, err)
	}
	r, err := b.agent.CalculatePrice(ctx, connect.NewRequest(&agentv1.CalculatePriceRequest{RuleName: "eu-landed-cost", InputJson: []byte(eximInputFOB)}))
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		CostMinor, PriceMinor int64
		MarginBps             int
		RequiresApproval      bool `json:"requires_approval"`
	}
	raw := map[string]any{}
	_ = json.Unmarshal(r.Msg.ResultJson, &raw)
	if raw["cost_minor"].(float64) != 912000 || raw["price_minor"].(float64) != 1112196 || raw["requires_approval"] != true {
		t.Fatalf("result = %v", raw)
	}
	_ = res

	for name, tc := range map[string]struct {
		rule, input string
		want        connect.Code
	}{
		"unknown rule":  {"nope", eximInputFOB, connect.CodeNotFound},
		"unknown field": {"eu-landed-cost", `{"exim":{"incoterm":"FOB","quantity":1,"unit_cost_minor":1,"hack":true}}`, connect.CodeInvalidArgument},
		"wrong model":   {"eu-landed-cost", `{"saas":{"tier":"a","seats":1,"billing":"monthly"}}`, connect.CodeInvalidArgument},
		"negative":      {"eu-landed-cost", `{"exim":{"incoterm":"FOB","quantity":1,"unit_cost_minor":-5}}`, connect.CodeInvalidArgument},
		"empty":         {"eu-landed-cost", ``, connect.CodeInvalidArgument},
	} {
		_, err := b.agent.CalculatePrice(ctx, connect.NewRequest(&agentv1.CalculatePriceRequest{RuleName: tc.rule, InputJson: []byte(tc.input)}))
		if code(err) != tc.want {
			t.Errorf("%s: code %v (%v), want %v", name, code(err), err, tc.want)
		}
	}
}

func TestQuoteIsQueuedWithEngineNumbersAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	b := goLive(t, e, "tok-founder", "kerala-spice-exports")

	q := &agentv1.CreateQuoteRequest{RuleName: "eu-landed-cost", InputJson: []byte(eximInputFOB), Customer: "Berlin Gewürze GmbH", IdempotencyKey: "q-1"}
	r1, err := b.agent.CreateQuote(ctx, connect.NewRequest(q))
	if err != nil || r1.Msg.Document.Status != "PENDING_APPROVAL" || !r1.Msg.Document.Created {
		t.Fatalf("quote: %+v %v", r1, err)
	}
	r2, err := b.agent.CreateQuote(ctx, connect.NewRequest(q))
	if err != nil || r2.Msg.Document.Created || r2.Msg.Document.ApprovalId != r1.Msg.Document.ApprovalId {
		t.Fatalf("replay must return the same approval: %+v %v", r2, err)
	}
	// Same key, different request: refused rather than silently overwritten.
	other := &agentv1.CreateQuoteRequest{RuleName: "eu-landed-cost", Customer: "Berlin Gewürze GmbH", IdempotencyKey: "q-1",
		InputJson: []byte(`{"exim":{"incoterm":"FOB","quantity":2000,"unit_cost_minor":800,"packing_minor":50000,"inland_freight_minor":30000}}`)}
	if _, err := b.agent.CreateQuote(ctx, connect.NewRequest(other)); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("key reuse: %v", err)
	}
	// A discount below the margin floor is queued as HIGH risk, never silently accepted or rejected.
	disc := &agentv1.CreateQuoteRequest{RuleName: "eu-landed-cost", Customer: "x", IdempotencyKey: "q-2",
		InputJson: []byte(`{"price_minor":950000,"exim":{"incoterm":"FOB","quantity":1000,"unit_cost_minor":800,"packing_minor":50000,"inland_freight_minor":30000}}`)}
	if _, err := b.agent.CreateQuote(ctx, connect.NewRequest(disc)); err != nil {
		t.Fatal(err)
	}
	list, err := e.approvals("tok-founder").ListApprovals(ctx, connect.NewRequest(&approvalv1.ListApprovalsRequest{}))
	if err != nil || len(list.Msg.Approvals) != 2 {
		t.Fatalf("inbox: %+v %v", list, err)
	}
	risks := map[string]string{}
	for _, a := range list.Msg.Approvals {
		risks[a.Title+a.RiskLevel] = a.RequestedBy
	}
	if list.Msg.Approvals[0].RiskLevel != "HIGH" || list.Msg.Approvals[1].RiskLevel != "MEDIUM" {
		t.Errorf("risks = %v", risks)
	}
	if list.Msg.Approvals[0].RequestedBy != "agent:sales" {
		t.Errorf("requested_by = %s", list.Msg.Approvals[0].RequestedBy)
	}
	// Input validation.
	bad := &agentv1.CreateQuoteRequest{RuleName: "eu-landed-cost", InputJson: []byte(eximInputFOB), Customer: "", IdempotencyKey: "q-3"}
	if _, err := b.agent.CreateQuote(ctx, connect.NewRequest(bad)); code(err) != connect.CodeInvalidArgument {
		t.Errorf("no customer: %v", err)
	}
}

func draft(counterparty, key, tax string, lines ...*agentv1.DocumentLine) *agentv1.DocumentDraft {
	return &agentv1.DocumentDraft{Counterparty: counterparty, TaxCode: tax, DueInDays: 30, IdempotencyKey: key, Lines: lines}
}

func TestDocumentsRequestApprovalAndGenericRequestsCannotForgeNumbers(t *testing.T) {
	e := newEnv(t)
	b := goLive(t, e, "tok-founder", "kerala-spice-exports")

	inv, err := b.agent.DraftInvoice(ctx, connect.NewRequest(&agentv1.DraftInvoiceRequest{Draft: draft("Berlin GmbH", "i-1", "GST5",
		&agentv1.DocumentLine{Description: "Black pepper", Quantity: 1000, UnitPriceMinor: 1112},
		&agentv1.DocumentLine{Description: "Freight", Quantity: 1, UnitPriceMinor: 12345})}))
	if err != nil {
		t.Fatal(err)
	}
	var totals struct {
		Subtotal int64 `json:"subtotal_minor"`
		Tax      int64 `json:"tax_minor"`
		Total    int64 `json:"total_minor"`
	}
	_ = json.Unmarshal(inv.Msg.Document.ComputedJson, &totals)
	if totals.Subtotal != 1124345 || totals.Tax != 56217 || totals.Total != 1180562 {
		t.Fatalf("totals = %+v", totals)
	}

	_, err = b.agent.DraftInvoice(ctx, connect.NewRequest(&agentv1.DraftInvoiceRequest{Draft: draft("x", "i-2", "VAT99", &agentv1.DocumentLine{Description: "a", Quantity: 1, UnitPriceMinor: 1})}))
	if code(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown tax code: %v", err)
	}
	_, err = b.agent.DraftPurchaseOrder(ctx, connect.NewRequest(&agentv1.DraftPurchaseOrderRequest{Draft: draft("x", "p-1", "GST0", &agentv1.DocumentLine{Description: "a", Quantity: 0, UnitPriceMinor: 1})}))
	if code(err) != connect.CodeInvalidArgument {
		t.Errorf("zero quantity: %v", err)
	}

	// An agent may not hand-write an invoice/quote/PO through the generic tool.
	for _, kind := range []string{"INVOICE", "QUOTE", "PURCHASE_ORDER", "BLUEPRINT", "bogus"} {
		_, err := b.agent.RequestApproval(ctx, connect.NewRequest(&agentv1.RequestApprovalRequest{Kind: kind, Title: "t", IdempotencyKey: "g-" + kind, PayloadJson: []byte(`{}`)}))
		if code(err) != connect.CodeInvalidArgument {
			t.Errorf("kind %s via generic request: %v", kind, err)
		}
	}
	// A payment must carry an amount, and is always HIGH risk even if the agent claims LOW.
	_, err = b.agent.RequestApproval(ctx, connect.NewRequest(&agentv1.RequestApprovalRequest{Kind: "PAYMENT", Title: "Pay freight", IdempotencyKey: "pay-0", PayloadJson: []byte(`{}`)}))
	if code(err) != connect.CodeInvalidArgument {
		t.Errorf("payment without amount: %v", err)
	}
	pay, err := b.agent.RequestApproval(ctx, connect.NewRequest(&agentv1.RequestApprovalRequest{
		Kind: "PAYMENT", Title: "Pay freight forwarder", RiskLevel: "LOW", AmountMinor: 120000, Currency: "INR", IdempotencyKey: "pay-1", PayloadJson: []byte(`{"payee":"Maersk"}`)}))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.approvals("tok-founder").GetApproval(ctx, connect.NewRequest(&approvalv1.GetApprovalRequest{TenantId: b.id, ApprovalId: pay.Msg.Approval.ApprovalId}))
	if got.Msg.Approval.RiskLevel != "HIGH" || got.Msg.Approval.AmountMinor != 120000 {
		t.Errorf("payment approval = %+v", got.Msg.Approval)
	}
	// Blockers are escalations the founder sees.
	esc, err := b.agent.EscalateBlocker(ctx, connect.NewRequest(&agentv1.EscalateBlockerRequest{Title: "No supplier quote", Details: "Cardamom price missing", Severity: "HIGH", IdempotencyKey: "b-1"}))
	if err != nil || esc.Msg.Approval.Status != "PENDING_APPROVAL" {
		t.Fatalf("escalate: %+v %v", esc, err)
	}
	st, err := b.agent.GetApprovalStatus(ctx, connect.NewRequest(&agentv1.GetApprovalStatusRequest{ApprovalId: esc.Msg.Approval.ApprovalId}))
	if err != nil || st.Msg.Approval.Status != "PENDING_APPROVAL" {
		t.Fatalf("status: %+v %v", st, err)
	}
}

func TestAgentsOfDifferentBusinessesNeverSeeEachOther(t *testing.T) {
	e := newEnv(t)
	a := goLive(t, e, "tok-founder", "kerala-spice-exports")
	b := goLive(t, e, "tok-other", "madurai-textiles")

	q, err := a.agent.CreateQuote(ctx, connect.NewRequest(&agentv1.CreateQuoteRequest{RuleName: "eu-landed-cost", InputJson: []byte(eximInputFOB), Customer: "c", IdempotencyKey: "q"}))
	if err != nil {
		t.Fatal(err)
	}
	// B's agent cannot read A's approval, and the same idempotency key in B creates a separate record.
	if _, err := b.agent.GetApprovalStatus(ctx, connect.NewRequest(&agentv1.GetApprovalStatusRequest{ApprovalId: q.Msg.Document.ApprovalId})); code(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant status: %v", err)
	}
	qb, err := b.agent.CreateQuote(ctx, connect.NewRequest(&agentv1.CreateQuoteRequest{RuleName: "eu-landed-cost", InputJson: []byte(eximInputFOB), Customer: "c", IdempotencyKey: "q"}))
	if err != nil || qb.Msg.Document.ApprovalId == q.Msg.Document.ApprovalId || !qb.Msg.Document.Created {
		t.Fatalf("same key in another tenant: %+v %v", qb, err)
	}
	// Founder A's inbox shows only A's business; B's founder sees only B's.
	la, _ := e.approvals("tok-founder").ListApprovals(ctx, connect.NewRequest(&approvalv1.ListApprovalsRequest{}))
	lb, _ := e.approvals("tok-other").ListApprovals(ctx, connect.NewRequest(&approvalv1.ListApprovalsRequest{}))
	if len(la.Msg.Approvals) != 1 || la.Msg.Approvals[0].TenantId != a.id || len(lb.Msg.Approvals) != 1 || lb.Msg.Approvals[0].TenantId != b.id {
		t.Fatalf("inboxes: A=%+v B=%+v", la.Msg.Approvals, lb.Msg.Approvals)
	}
	// B's founder cannot read or decide A's approval; the answer is NotFound, not PermissionDenied.
	_, err = e.approvals("tok-other").GetApproval(ctx, connect.NewRequest(&approvalv1.GetApprovalRequest{TenantId: a.id, ApprovalId: q.Msg.Document.ApprovalId}))
	if code(err) != connect.CodeNotFound {
		t.Errorf("cross-tenant get: %v", err)
	}
	_, err = e.approvals("tok-other").DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: a.id, ApprovalId: q.Msg.Document.ApprovalId, Approve: false}))
	if code(err) != connect.CodeNotFound {
		t.Errorf("cross-tenant decide: %v", err)
	}
}

func approvalHash(t *testing.T, e *env, token, tenant, id string) string {
	t.Helper()
	g, err := e.approvals(token).GetApproval(ctx, connect.NewRequest(&approvalv1.GetApprovalRequest{TenantId: tenant, ApprovalId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return g.Msg.PayloadHash
}

func TestApprovingAnInvoicePostsTheLedgerExactlyOnce(t *testing.T) {
	e := newEnv(t)
	b := goLive(t, e, "tok-founder", "kerala-spice-exports")
	inv, err := b.agent.DraftInvoice(ctx, connect.NewRequest(&agentv1.DraftInvoiceRequest{Draft: draft("Berlin GmbH", "i-1", "GST5",
		&agentv1.DocumentLine{Description: "Black pepper", Quantity: 1000, UnitPriceMinor: 1112},
		&agentv1.DocumentLine{Description: "Freight", Quantity: 1, UnitPriceMinor: 12345})}))
	if err != nil {
		t.Fatal(err)
	}
	id := inv.Msg.Document.ApprovalId
	founder := e.approvals("tok-founder")

	// Nothing has been posted before approval.
	if bal, _ := e.store.AccountBalance(ctx, b.uuid, "1200"); bal != 0 {
		t.Fatalf("receivable before approval = %d", bal)
	}
	// A stale / wrong hash cannot approve; the request stays pending.
	_, err = founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: id, Approve: true, PayloadHash: "00" + approvalHash(t, e, "tok-founder", b.id, id)[2:]}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("wrong hash: %v", err)
	}
	_, err = founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: id, Approve: true}))
	if code(err) != connect.CodeInvalidArgument {
		t.Fatalf("missing hash: %v", err)
	}
	if st, _ := b.agent.GetApprovalStatus(ctx, connect.NewRequest(&agentv1.GetApprovalStatusRequest{ApprovalId: id})); st.Msg.Approval.Status != "PENDING_APPROVAL" {
		t.Fatalf("status after refused approvals = %s", st.Msg.Approval.Status)
	}

	hash := approvalHash(t, e, "tok-founder", b.id, id)
	// Two founders click at once: exactly one wins; the ledger is posted once.
	var wg sync.WaitGroup
	results := make([]error, 2)
	var executed int
	var mu sync.Mutex
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: id, Approve: true, PayloadHash: hash, Note: "ok"}))
			results[i] = err
			if err == nil && r.Msg.Executed {
				mu.Lock()
				executed++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	// A duplicate click is either refused as already decided, or (when it lands between the other click's decision and
	// its execution) it finishes the job instead; the invariant is that the side effect happens exactly once.
	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		} else if code(err) != connect.CodeFailedPrecondition {
			t.Errorf("loser got %v", err)
		}
	}
	if wins < 1 || executed != 1 {
		t.Fatalf("wins=%d executed=%d, want at least one success and exactly one execution (%v)", wins, executed, results)
	}

	// Dr receivable 1,180,562 / Cr revenue 1,124,345 / Cr GST payable 56,217; one scheduled receipt.
	for code, want := range map[string]int64{"1200": 1180562, "4000": -1124345, "2200": -56217} {
		if got, _ := e.store.AccountBalance(ctx, b.uuid, code); got != want {
			t.Errorf("account %s balance = %d, want %d", code, got, want)
		}
	}
	st, _ := b.agent.GetApprovalStatus(ctx, connect.NewRequest(&agentv1.GetApprovalStatusRequest{ApprovalId: id}))
	if st.Msg.Approval.Status != "EXECUTED" {
		t.Fatalf("final status = %s", st.Msg.Approval.Status)
	}
	// Deciding again is refused and posts nothing more.
	_, err = founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: id, Approve: true, PayloadHash: hash}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("second decision: %v", err)
	}
	if got, _ := e.store.AccountBalance(ctx, b.uuid, "1200"); got != 1180562 {
		t.Fatalf("receivable after replay = %d", got)
	}

	// The receipt is now part of the cash forecast (due in 30 days, committed), and the opening balance is the ledger's.
	f, err := b.agent.ForecastCashFlow(ctx, connect.NewRequest(&agentv1.ForecastCashFlowRequest{Days: 60, BucketDays: 30}))
	if err != nil {
		t.Fatal(err)
	}
	var fc struct {
		Opening int64 `json:"opening_minor"`
		Ending  int64 `json:"ending_minor"`
		Runway  *int  `json:"runway_days"`
	}
	_ = json.Unmarshal(f.Msg.ForecastJson, &fc)
	if fc.Opening != 500000000 || fc.Ending != 500000000+1180562 || fc.Runway != nil || f.Msg.Currency != "INR" {
		t.Fatalf("forecast = %+v currency=%s", fc, f.Msg.Currency)
	}
}

func TestDecisionsRolesRejectionAndKindsWithoutExecutors(t *testing.T) {
	e := newEnv(t)
	b := goLive(t, e, "tok-founder", "kerala-spice-exports")
	if err := e.store.AddMember(ctx, b.uuid, "sub-viewer", "viewer"); err != nil {
		t.Fatal(err)
	}
	po, _ := b.agent.DraftPurchaseOrder(ctx, connect.NewRequest(&agentv1.DraftPurchaseOrderRequest{Draft: draft("Farm Co-op", "p-1", "GST0", &agentv1.DocumentLine{Description: "Raw pepper", Quantity: 500, UnitPriceMinor: 600})}))
	pay, _ := b.agent.RequestApproval(ctx, connect.NewRequest(&agentv1.RequestApprovalRequest{Kind: "PAYMENT", Title: "Pay", AmountMinor: 1, Currency: "INR", IdempotencyKey: "pay", PayloadJson: []byte(`{}`)}))
	quote, _ := b.agent.CreateQuote(ctx, connect.NewRequest(&agentv1.CreateQuoteRequest{RuleName: "eu-landed-cost", InputJson: []byte(eximInputFOB), Customer: "c", IdempotencyKey: "q"}))
	founder, viewer := e.approvals("tok-founder"), e.approvals("tok-viewer")

	// A viewer sees nothing in the inbox and cannot decide.
	vl, err := viewer.ListApprovals(ctx, connect.NewRequest(&approvalv1.ListApprovalsRequest{}))
	if err != nil || len(vl.Msg.Approvals) != 0 {
		t.Fatalf("viewer inbox: %+v %v", vl, err)
	}
	_, err = viewer.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: po.Msg.Document.ApprovalId, Approve: false}))
	if code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer decide: %v", err)
	}

	// Approving a PO schedules an outflow; it posts no journal (a commitment, not a liability yet).
	r, err := founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{
		TenantId: b.id, ApprovalId: po.Msg.Document.ApprovalId, Approve: true, PayloadHash: approvalHash(t, e, "tok-founder", b.id, po.Msg.Document.ApprovalId)}))
	if err != nil || !r.Msg.Executed || r.Msg.Status != "EXECUTED" {
		t.Fatalf("po: %+v %v", r, err)
	}
	if bal, _ := e.store.AccountBalance(ctx, b.uuid, "2100"); bal != 0 {
		t.Errorf("PO must not touch payables yet: %d", bal)
	}
	f, _ := b.agent.ForecastCashFlow(ctx, connect.NewRequest(&agentv1.ForecastCashFlowRequest{Days: 45}))
	var fc struct {
		Ending int64 `json:"ending_minor"`
	}
	_ = json.Unmarshal(f.Msg.ForecastJson, &fc)
	if fc.Ending != 500000000-300000 {
		t.Errorf("forecast ending = %d, want PO outflow of 300,000 applied", fc.Ending)
	}

	// A payment is approved but NOT executed: no payment connector exists, and the system says so.
	pr, err := founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{
		TenantId: b.id, ApprovalId: pay.Msg.Approval.ApprovalId, Approve: true, PayloadHash: approvalHash(t, e, "tok-founder", b.id, pay.Msg.Approval.ApprovalId)}))
	if err != nil || pr.Msg.Executed || pr.Msg.Status != "APPROVED" {
		t.Fatalf("payment: %+v %v", pr, err)
	}

	// Rejection needs no hash, executes nothing and is final.
	rr, err := founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: quote.Msg.Document.ApprovalId, Approve: false, Note: "price too low"}))
	if err != nil || rr.Msg.Status != "REJECTED" || rr.Msg.Executed {
		t.Fatalf("reject: %+v %v", rr, err)
	}
	st, _ := b.agent.GetApprovalStatus(ctx, connect.NewRequest(&agentv1.GetApprovalStatusRequest{ApprovalId: quote.Msg.Document.ApprovalId}))
	if st.Msg.Approval.Status != "REJECTED" || st.Msg.Approval.DecisionNote != "price too low" {
		t.Fatalf("agent view of rejection: %+v", st.Msg.Approval)
	}
	_, err = founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: quote.Msg.Document.ApprovalId, Approve: true, PayloadHash: approvalHash(t, e, "tok-founder", b.id, quote.Msg.Document.ApprovalId)}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("approving a rejected request: %v", err)
	}

}

func TestPortfolioShowsCashRunwayAndPendingPerBusiness(t *testing.T) {
	e := newEnv(t)
	live := goLive(t, e, "tok-founder", "kerala-spice-exports")
	f := e.as("tok-founder")
	draftBiz, err := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "second"}))
	if err != nil {
		t.Fatal(err)
	}
	portfolio := portfoliov1connect.NewPortfolioServiceClient(http.DefaultClient, e.url, connect.WithInterceptors(bearer("tok-founder")))
	get := func() map[string]*portfoliov1.BusinessHealth {
		r, err := portfolio.GetPortfolio(ctx, connect.NewRequest(&portfoliov1.GetPortfolioRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]*portfoliov1.BusinessHealth{}
		for _, b := range r.Msg.Businesses {
			m[b.TenantId] = b
		}
		return m
	}

	rows := get()
	a, d := rows[live.id], rows[draftBiz.Msg.TenantId]
	if len(rows) != 2 || a == nil || d == nil {
		t.Fatalf("rows = %+v", rows)
	}
	if a.Status != "ACTIVE" || !a.ForecastAvailable || a.CashMinor != 500000000 || a.RunwayDays != 0 || a.CashNegativeWithin_90D ||
		a.LastProvisionStatus != "SUCCEEDED" || a.BaseCurrency != "INR" || a.Role != "founder" {
		t.Fatalf("active business = %+v", a)
	}
	if d.Status != "DRAFT" || d.ForecastAvailable || d.CashMinor != 0 || d.LastProvisionStatus != "" {
		t.Fatalf("draft business = %+v", d)
	}

	// Two pending items (one HIGH: payments always are) show up in the counts.
	live.agent.CreateQuote(ctx, connect.NewRequest(&agentv1.CreateQuoteRequest{RuleName: "eu-landed-cost", InputJson: []byte(eximInputFOB), Customer: "c", IdempotencyKey: "q"}))
	live.agent.RequestApproval(ctx, connect.NewRequest(&agentv1.RequestApprovalRequest{Kind: "PAYMENT", Title: "p", AmountMinor: 1, Currency: "INR", IdempotencyKey: "p", PayloadJson: []byte(`{}`)}))
	if a := get()[live.id]; a.PendingApprovals != 2 || a.HighRiskPending != 1 {
		t.Fatalf("pending = %d high = %d", a.PendingApprovals, a.HighRiskPending)
	}

	// An approved purchase order bigger than the cash on hand, due in 10 days, puts the business on a runway.
	po, err := live.agent.DraftPurchaseOrder(ctx, connect.NewRequest(&agentv1.DraftPurchaseOrderRequest{Draft: &agentv1.DocumentDraft{
		Counterparty: "Big Supplier", TaxCode: "GST0", DueInDays: 10, IdempotencyKey: "po",
		Lines: []*agentv1.DocumentLine{{Description: "Bulk order", Quantity: 1, UnitPriceMinor: 600000000}}}}))
	if err != nil {
		t.Fatal(err)
	}
	hash := approvalHash(t, e, "tok-founder", live.id, po.Msg.Document.ApprovalId)
	if _, err := e.approvals("tok-founder").DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{
		TenantId: live.id, ApprovalId: po.Msg.Document.ApprovalId, Approve: true, PayloadHash: hash})); err != nil {
		t.Fatal(err)
	}
	a = get()[live.id]
	if !a.CashNegativeWithin_90D || a.RunwayDays != 11 || a.MinCashMinor_90D != 500000000-600000000 {
		t.Fatalf("after PO: %+v", a)
	}

	// Another user's portfolio is empty: they are not a member of anything.
	other := portfoliov1connect.NewPortfolioServiceClient(http.DefaultClient, e.url, connect.WithInterceptors(bearer("tok-other")))
	if r, err := other.GetPortfolio(ctx, connect.NewRequest(&portfoliov1.GetPortfolioRequest{})); err != nil || len(r.Msg.Businesses) != 0 {
		t.Fatalf("outsider portfolio: %+v %v", r, err)
	}
}
