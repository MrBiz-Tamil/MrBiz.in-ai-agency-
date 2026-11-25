package mcpbus_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	tenantv1 "github.com/mrbiz/venture-os/gen/venture/tenant/v1"
	"github.com/mrbiz/venture-os/gen/venture/tenant/v1/tenantv1connect"
	"github.com/mrbiz/venture-os/internal/mcpbus"
	"github.com/mrbiz/venture-os/internal/testenv"
)

// e2e wires the real chain: MCP JSON-RPC -> bus -> ConnectRPC -> control plane -> Postgres (RLS enforced).
type e2e struct {
	stack *testenv.Stack
	bus   *httptest.Server
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	stack := testenv.New(t)
	agent := agentv1connect.NewAgentServiceClient(http.DefaultClient, stack.URL)
	bus := httptest.NewServer(mcpbus.New(mcpbus.Server{Agent: agent, Signer: stack.Signer}))
	t.Cleanup(bus.Close)
	return &e2e{stack: stack, bus: bus}
}

func (e *e2e) call(t *testing.T, token, tool string, args any) (text string, isErr bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequest(http.MethodPost, e.bus.URL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r rpc
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Error != nil {
		t.Fatalf("%s: %v %+v", tool, err, r.Error)
	}
	var out toolOut
	_ = json.Unmarshal(r.Result, &out)
	return out.Content[0].Text, out.IsError
}

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("not JSON: %q", text)
	}
	return m
}

const fobArgs = `{"rule_name":"eu-landed-cost","exim":{"incoterm":"FOB","quantity":1000,"unit_cost_minor":800,"packing_minor":50000,"inland_freight_minor":30000}}`

func args(t *testing.T, raw string, extra map[string]any) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestAgentJourneyThroughMCP(t *testing.T) {
	e := newE2E(t)
	b := e.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")

	// Read-only pricing: engine numbers, never the agent's.
	text, isErr := e.call(t, b.AgentToken, "calculate_price", args(t, fobArgs, nil))
	if isErr {
		t.Fatal(text)
	}
	res := decode(t, text)
	if res["cost_minor"].(float64) != 912000 || res["price_minor"].(float64) != 1112196 {
		t.Fatalf("calculate_price = %v", res)
	}

	// A quote is queued, not sent; the tool says so in words an agent will understand.
	text, isErr = e.call(t, b.AgentToken, "create_quote", args(t, fobArgs, map[string]any{"customer": "Berlin Gewürze GmbH", "idempotency_key": "quote-1"}))
	if isErr {
		t.Fatal(text)
	}
	q := decode(t, text)
	qid := q["approval_id"].(string)
	if q["status"] != "PENDING_APPROVAL" || !strings.Contains(q["message"].(string), "NOTHING has been sent") {
		t.Fatalf("quote = %v", q)
	}
	// Retrying with the same key returns the same approval.
	text, _ = e.call(t, b.AgentToken, "create_quote", args(t, fobArgs, map[string]any{"customer": "Berlin Gewürze GmbH", "idempotency_key": "quote-1"}))
	if again := decode(t, text); again["approval_id"] != qid || again["created"] != false {
		t.Fatalf("retry = %v", again)
	}

	// The agent sees PENDING, then the founder approves and the agent sees EXECUTED.
	text, _ = e.call(t, b.AgentToken, "get_approval_status", map[string]any{"approval_id": qid})
	if decode(t, text)["status"] != "PENDING_APPROVAL" {
		t.Fatalf("status = %s", text)
	}
	if r := e.stack.Decide(t, "tok-founder", b.ID, qid, true); !r.Executed {
		t.Fatalf("decision = %+v", r)
	}
	text, _ = e.call(t, b.AgentToken, "get_approval_status", map[string]any{"approval_id": qid})
	if decode(t, text)["status"] != "EXECUTED" {
		t.Fatalf("status after approval = %s", text)
	}

	// An invoice: totals come from the engine; approval posts to the real ledger.
	text, isErr = e.call(t, b.AgentToken, "draft_invoice", map[string]any{
		"counterparty": "Berlin GmbH", "tax_code": "GST5", "due_in_days": 30, "idempotency_key": "inv-1",
		"lines": []map[string]any{{"description": "Black pepper", "quantity": 1000, "unit_price_minor": 1112}, {"description": "Freight", "quantity": 1, "unit_price_minor": 12345}},
	})
	if isErr {
		t.Fatal(text)
	}
	inv := decode(t, text)
	if inv["totals"].(map[string]any)["total_minor"].(float64) != 1180562 {
		t.Fatalf("invoice = %v", inv)
	}
	if bal, _ := e.stack.Store.AccountBalance(context.Background(), b.UUID, "1200"); bal != 0 {
		t.Fatalf("ledger touched before approval: %d", bal)
	}
	e.stack.Decide(t, "tok-founder", b.ID, inv["approval_id"].(string), true)
	if bal, _ := e.stack.Store.AccountBalance(context.Background(), b.UUID, "1200"); bal != 1180562 {
		t.Fatalf("receivable after approval = %d", bal)
	}

	// The forecast reflects the ledger cash and the committed receipt.
	text, isErr = e.call(t, b.AgentToken, "forecast_cash_flow", map[string]any{"days": 60})
	if isErr {
		t.Fatal(text)
	}
	f := decode(t, text)
	fc := f["forecast"].(map[string]any)
	if f["currency"] != "INR" || fc["opening_minor"].(float64) != 500000000 || fc["ending_minor"].(float64) != 500000000+1180562 {
		t.Fatalf("forecast = %v", f)
	}

	// Payments are requested through the generic tool; approval records the decision but cannot move money.
	text, isErr = e.call(t, b.AgentToken, "request_human_approval", map[string]any{
		"kind": "PAYMENT", "title": "Pay ocean freight", "summary": "Maersk invoice 4411", "amount_minor": 120000, "currency": "INR",
		"payload": map[string]any{"payee": "Maersk"}, "idempotency_key": "pay-1"})
	if isErr {
		t.Fatal(text)
	}
	pid := decode(t, text)["approval_id"].(string)
	if r := e.stack.Decide(t, "tok-founder", b.ID, pid, true); r.Executed || r.Status != "APPROVED" {
		t.Fatalf("payment decision = %+v", r)
	}

	// Escalations reach the founder's inbox too.
	text, isErr = e.call(t, b.AgentToken, "escalate_blocker", map[string]any{"title": "No cardamom quote", "details": "Supplier unresponsive for 5 days", "severity": "HIGH", "idempotency_key": "blk-1"})
	if isErr || decode(t, text)["status"] != "PENDING_APPROVAL" {
		t.Fatalf("escalate = %s", text)
	}
	list, err := e.stack.Approvals("tok-founder").ListApprovals(context.Background(), connect.NewRequest(approvalListReq()))
	if err != nil || len(list.Msg.Approvals) != 1 || list.Msg.Approvals[0].Kind != "OTHER" || list.Msg.Approvals[0].RiskLevel != "HIGH" {
		t.Fatalf("inbox = %+v %v", list, err)
	}
}

func TestBusEnforcesTenantIsolationEndToEnd(t *testing.T) {
	e := newE2E(t)
	a := e.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	b := e.stack.LiveBusiness(t, "tok-other", "madurai-textiles")

	text, _ := e.call(t, a.AgentToken, "create_quote", args(t, fobArgs, map[string]any{"customer": "c", "idempotency_key": "q"}))
	aid := decode(t, text)["approval_id"].(string)

	// B's agent cannot read A's approval, and cannot even tell it exists.
	text, isErr := e.call(t, b.AgentToken, "get_approval_status", map[string]any{"approval_id": aid})
	if !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("cross-tenant read: %q isErr=%v", text, isErr)
	}
	// Smuggling a tenant id is rejected before it reaches the control plane.
	text, isErr = e.call(t, b.AgentToken, "get_approval_status", map[string]any{"approval_id": aid, "tenant_id": a.ID})
	if !isErr || !strings.Contains(text, "Invalid arguments") {
		t.Fatalf("tenant smuggling: %q", text)
	}
	// Each business's inbox holds only its own items, even with identical idempotency keys.
	e.call(t, b.AgentToken, "create_quote", args(t, fobArgs, map[string]any{"customer": "c", "idempotency_key": "q"}))
	for tok, tenant := range map[string]string{"tok-founder": a.ID, "tok-other": b.ID} {
		l, err := e.stack.Approvals(tok).ListApprovals(context.Background(), connect.NewRequest(approvalListReq()))
		if err != nil || len(l.Msg.Approvals) != 1 || l.Msg.Approvals[0].TenantId != tenant {
			t.Fatalf("%s inbox = %+v %v", tok, l, err)
		}
	}
}

func TestToolsAreUnavailableUntilTheBusinessIsLive(t *testing.T) {
	e := newE2E(t)
	opt := connect.WithInterceptors(bearerOIDC("tok-founder"))
	tenants := tenantv1connect.NewTenantServiceClient(http.DefaultClient, e.stack.URL, opt)
	cb, err := tenants.CreateBusiness(context.Background(), connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "k"}))
	if err != nil {
		t.Fatal(err)
	}
	tid := mustUUID(t, cb.Msg.TenantId)
	tok, _ := e.stack.Signer.Mint(tid, "", time.Hour)
	text, isErr := e.call(t, tok, "list_pricing_rules", map[string]any{})
	if !isErr || !strings.Contains(text, "not ACTIVE") {
		t.Fatalf("draft business: %q isErr=%v", text, isErr)
	}
}
