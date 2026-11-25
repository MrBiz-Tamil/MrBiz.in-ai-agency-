package graphql_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"

	"github.com/mrbiz/abos/backend-core/pkg/auth"
	"github.com/mrbiz/abos/backend-core/pkg/db/dbtest"
	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/events"
	agentv1 "github.com/mrbiz/abos/backend-core/pkg/gen/abos/agent/v1"
	gql "github.com/mrbiz/abos/backend-core/pkg/graphql"
	"github.com/mrbiz/abos/backend-core/pkg/kernel"
	"github.com/mrbiz/abos/backend-core/pkg/mcp"
	"github.com/mrbiz/abos/backend-core/pkg/tickets"
	"github.com/mrbiz/abos/backend-core/pkg/vault"
)

type fakeExec struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeExec) Execute(context.Context, string, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return `{"status":"success"}`, nil
}

type fakeAI struct{ policyErr error }

func (f fakeAI) DecomposeGoal(_ context.Context, _, _, _ string, _ map[string]string) (*agentv1.DecomposeGoalResponse, error) {
	return &agentv1.DecomposeGoalResponse{ProjectedMarginPct: 26.4, EstimatedDays: 21, Milestones: []*agentv1.MilestonePlan{
		{StepOrder: 1, AssignedAgentRole: "Router", ToolAction: "inventory.sync_buffer", CronSchedule: "0 */4 * * *"},
		{StepOrder: 2, AssignedAgentRole: "Finance", ToolAction: "books.match_payout"},
	}}, nil
}
func (f fakeAI) CompilePolicy(_ context.Context, _, text string) (*agentv1.CompilePolicyResponse, error) {
	if f.policyErr != nil {
		return nil, f.policyErr
	}
	return &agentv1.CompilePolicyResponse{RuleId: "pol_test1", TriggerEvent: "order.refund_requested", AssignedApprovalRole: "Finance",
		CompiledAstJson: `{"rule_id":"pol_test1","trigger_event":"order.refund_requested","conditions":[{"field":"refund_amount","operator":"GREATER_THAN","threshold":200}],"action":"SUSPEND_FOR_APPROVAL","assigned_role":"Finance"}`}, nil
}
func (f fakeAI) RunAgentHeartbeat(context.Context, string, string, string) (*agentv1.RunAgentHeartbeatResponse, error) {
	return &agentv1.RunAgentHeartbeatResponse{Status: "HEALTHY"}, nil
}

type api struct {
	t        *testing.T
	repo     *repository.Repository
	svc      *auth.Service
	ts       *httptest.Server
	exec     *fakeExec
	wsA, wsB string
	tokens   map[string]string // role/ws label -> session token
	agentA   *repository.AgentDefinition
	policy   *kernel.PolicyEvaluator
	tkSvc    *tickets.Service
}

const password = "correct horse battery"

func newAPI(t *testing.T) *api {
	t.Helper()
	pool := dbtest.Pool(t)
	repo := repository.NewRepository(pool)
	ctx := context.Background()

	a, err := repo.CreateWorkspace(ctx, "acme", "INR", "GST_INDIA", decimal.NewFromInt(100))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := repo.CreateWorkspace(ctx, "globex", "INR", "GST_INDIA", decimal.NewFromInt(100))

	hash, _ := auth.HashPassword(password)
	mk := func(ws, email, role string) {
		if _, err := repo.CreateUser(ctx, repository.User{WorkspaceID: ws, Email: email, Role: role, PasswordHash: hash}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a.ID, "owner@a.test", "owner")
	mk(a.ID, "operator@a.test", "operator")
	mk(a.ID, "auditor@a.test", "auditor")
	mk(b.ID, "owner@b.test", "owner")

	svc := auth.NewService(repo)
	tokens := map[string]string{}
	for label, email := range map[string]string{"ownerA": "owner@a.test", "operatorA": "operator@a.test", "auditorA": "auditor@a.test", "ownerB": "owner@b.test"} {
		tok, _, _, err := svc.Login(ctx, email, password, "test")
		if err != nil {
			t.Fatal(err)
		}
		tokens[label] = tok
	}

	cron := "*/15 * * * *"
	agent, err := repo.CreateAgent(ctx, repository.AgentDefinition{WorkspaceID: a.ID, RoleName: "Router", ModelTier: "FLASH", HeartbeatCron: &cron, DailyBudget: decimal.NewFromInt(5)})
	if err != nil {
		t.Fatal(err)
	}

	broker := events.NewBroker()
	policy := kernel.NewPolicyEvaluator()
	exec := &fakeExec{}
	tk := tickets.NewService(repo, policy, exec, broker)
	v, _ := vault.NewRandom()
	gw := mcp.NewGateway(repo, v, &mcp.Client{HTTP: mcp.NewHTTPClient(true)})

	res := &gql.Resolver{Repo: repo, Tickets: tk, Broker: broker, Policy: policy, Gateway: gw, Vault: v, AI: fakeAI{}, AllowPrivateMCP: true}
	srv := gql.NewServer(res, gql.ServerOptions{Introspection: false, Auth: svc, CheckOrigin: func(*http.Request) bool { return true }})
	mux := http.NewServeMux()
	mux.Handle("/graphql", svc.Middleware(srv))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return &api{t: t, repo: repo, svc: svc, ts: ts, exec: exec, wsA: a.ID, wsB: b.ID, tokens: tokens, agentA: agent, policy: policy, tkSvc: tk}
}

type gqlResp struct {
	Data   map[string]any `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func (a *api) do(token, query string, vars map[string]any) (int, gqlResp) {
	a.t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequest(http.MethodPost, a.ts.URL+"/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+tokens(a, token))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out gqlResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func tokens(a *api, label string) string {
	if t, ok := a.tokens[label]; ok {
		return t
	}
	return label
}

func (a *api) ok(token, query string, vars map[string]any) map[string]any {
	a.t.Helper()
	code, r := a.do(token, query, vars)
	if code != 200 || len(r.Errors) > 0 {
		a.t.Fatalf("expected success, got %d %+v", code, r.Errors)
	}
	return r.Data
}

func (a *api) fail(token, query string, vars map[string]any, wantCode string) {
	a.t.Helper()
	_, r := a.do(token, query, vars)
	if len(r.Errors) == 0 {
		a.t.Fatalf("expected error %s, got success: %v", wantCode, r.Data)
	}
	if got, _ := r.Errors[0].Extensions["code"].(string); got != wantCode {
		a.t.Fatalf("expected code %s, got %s (%s)", wantCode, got, r.Errors[0].Message)
	}
}

func (a *api) seedRefund(key string, amount string) *repository.ExecutionTicket {
	a.t.Helper()
	tk, err := a.tkSvc.Submit(context.Background(), tickets.SubmitInput{WorkspaceID: a.wsA, IdempotencyKey: key, ActionType: "order.refund_requested",
		Payload: json.RawMessage(`{"order_id":"o1","refund_amount":"` + amount + `"}`)})
	if err != nil {
		a.t.Fatal(err)
	}
	return tk
}

func TestAuthentication(t *testing.T) {
	a := newAPI(t)
	q := `{ workspace(id:"` + a.wsA + `") { name } }`
	if code, _ := a.do("", q, nil); code != 401 {
		t.Errorf("no token must be 401, got %d", code)
	}
	if code, _ := a.do("garbage-token", q, nil); code != 401 {
		t.Errorf("bad token must be 401, got %d", code)
	}
	d := a.ok("ownerA", q, nil)
	if d["workspace"].(map[string]any)["name"] != "acme" {
		t.Errorf("workspace: %v", d)
	}
}

func TestTenantIsolation(t *testing.T) {
	a := newAPI(t)
	tk := a.seedRefund("iso", "5000")

	// B's owner names A's workspace explicitly: forbidden.
	a.fail("ownerB", `{ executionBoard(workspaceId:"`+a.wsA+`") { incomingCount } }`, nil, "FORBIDDEN")
	a.fail("ownerB", `{ tickets(workspaceId:"`+a.wsA+`") { id } }`, nil, "FORBIDDEN")
	a.fail("ownerB", `{ agentRoster(workspaceId:"`+a.wsA+`") { id } }`, nil, "FORBIDDEN")
	// B's owner asks for A's objects by id: not found, never the data.
	a.fail("ownerB", `{ ticket(id:"`+tk.ID+`") { id } }`, nil, "NOT_FOUND")
	a.fail("ownerB", `{ decisionXRay(ticketId:"`+tk.ID+`") { id } }`, nil, "NOT_FOUND")
	a.fail("ownerB", `{ agent(id:"`+a.agentA.ID+`") { id } }`, nil, "NOT_FOUND")
	a.fail("ownerB", `mutation { approveTicket(input:{ticketId:"`+tk.ID+`"}) { id } }`, nil, "NOT_FOUND")
	a.fail("ownerB", `mutation { pauseAgent(agentId:"`+a.agentA.ID+`") { id } }`, nil, "NOT_FOUND")
	if a.exec.calls != 0 {
		t.Error("a foreign workspace must never trigger execution")
	}
	// B sees its own empty board.
	d := a.ok("ownerB", `{ executionBoard(workspaceId:"`+a.wsB+`") { pendingApprovalsCount recentTickets { id } } }`, nil)
	if d["executionBoard"].(map[string]any)["pendingApprovalsCount"].(float64) != 0 {
		t.Errorf("B must not see A's pending approvals: %v", d)
	}
}

func TestRoles(t *testing.T) {
	a := newAPI(t)
	tk := a.seedRefund("roles", "5000")
	approve := `mutation { approveTicket(input:{ticketId:"` + tk.ID + `"}) { status } }`

	a.fail("auditorA", approve, nil, "FORBIDDEN")
	a.fail("auditorA", `mutation { pauseAgent(agentId:"`+a.agentA.ID+`") { id } }`, nil, "FORBIDDEN")
	a.fail("operatorA", `mutation { updateAgentBudget(agentId:"`+a.agentA.ID+`", dailyBudgetUsd:"50") { id } }`, nil, "FORBIDDEN")
	a.fail("operatorA", `mutation { compileAndSavePolicy(input:{workspaceId:"`+a.wsA+`", rawPolicyText:"refund above 100"}) { id } }`, nil, "FORBIDDEN")

	// Auditors can read everything, including the audit trail.
	a.ok("auditorA", `{ pendingApprovals(workspaceId:"`+a.wsA+`") { id status } decisionXRay(ticketId:"`+tk.ID+`") { toolName policyDecision } }`, nil)

	if a.exec.calls != 0 {
		t.Fatal("forbidden calls must not execute anything")
	}
	d := a.ok("operatorA", approve, nil)
	if d["approveTicket"].(map[string]any)["status"] != "COMPLETED" || a.exec.calls != 1 {
		t.Errorf("operator approval: %v calls=%d", d, a.exec.calls)
	}
}

func TestApprovalFlowAndXRay(t *testing.T) {
	a := newAPI(t)
	tk := a.seedRefund("flow", "1500")

	d := a.ok("operatorA", `{ pendingApprovals(workspaceId:"`+a.wsA+`") { id status actionType conflictDiff { field valueA systemB valueB entityType } } }`, nil)
	list := d["pendingApprovals"].([]any)
	if len(list) != 1 {
		t.Fatalf("expected one pending ticket: %v", d)
	}
	cd := list[0].(map[string]any)["conflictDiff"].(map[string]any)
	if cd["field"] != "refund_amount" || cd["valueA"] != "1500" || cd["entityType"] != "ORDER" || !strings.Contains(cd["valueB"].(string), "rule_refund_threshold_01") {
		t.Errorf("conflict diff: %v", cd)
	}

	a.fail("operatorA", `mutation { rejectTicket(input:{ticketId:"`+tk.ID+`", rejectionReason:""}) { id } }`, nil, "BAD_USER_INPUT")
	a.ok("operatorA", `mutation { approveTicket(input:{ticketId:"`+tk.ID+`", overrideReason:"checked"}) { status result } }`, nil)
	a.fail("operatorA", `mutation { approveTicket(input:{ticketId:"`+tk.ID+`"}) { id } }`, nil, "CONFLICT")
	if a.exec.calls != 1 {
		t.Errorf("executed %d times", a.exec.calls)
	}

	d = a.ok("auditorA", `{ decisionXRay(ticketId:"`+tk.ID+`") { stepNumber toolName policyDecision outputSanitized } }`, nil)
	steps := d["decisionXRay"].([]any)
	if len(steps) != 3 {
		t.Fatalf("expected 3 audit steps, got %d: %v", len(steps), steps)
	}
	first := steps[0].(map[string]any)
	if first["toolName"] != "kernel.policy_evaluator" || first["policyDecision"] != "SUSPEND_FOR_APPROVAL" {
		t.Errorf("first step: %v", first)
	}
	appr := steps[1].(map[string]any)["outputSanitized"].(map[string]any)
	if appr["by"] != "operator@a.test" || appr["reason"] != "checked" {
		t.Errorf("approval must record who approved and why: %v", appr)
	}

	// Modify & execute on a second ticket
	tk2 := a.seedRefund("flow2", "9000")
	d = a.ok("operatorA", `mutation { modifyAndExecuteTicket(input:{ticketId:"`+tk2.ID+`", modifiedPayload:{order_id:"o1", refund_amount:"800"}}) { status payload } }`, nil)
	if d["modifyAndExecuteTicket"].(map[string]any)["payload"].(map[string]any)["refund_amount"] != "800" {
		t.Errorf("modify: %v", d)
	}
}

func TestAgentsBudgetsAndHeartbeat(t *testing.T) {
	a := newAPI(t)
	id := a.agentA.ID
	d := a.ok("ownerA", `mutation { updateAgentBudget(agentId:"`+id+`", dailyBudgetUsd:"12.5") { dailyBudgetUsd } }`, nil)
	if d["updateAgentBudget"].(map[string]any)["dailyBudgetUsd"] != "12.5" {
		t.Errorf("budget: %v", d)
	}
	for _, bad := range []string{"-1", "abc", "999999999"} {
		a.fail("ownerA", `mutation { updateAgentBudget(agentId:"`+id+`", dailyBudgetUsd:"`+bad+`") { id } }`, nil, "BAD_USER_INPUT")
	}
	a.ok("operatorA", `mutation { pauseAgent(agentId:"`+id+`") { status } }`, nil)
	a.fail("operatorA", `mutation { triggerManualHeartbeat(agentId:"`+id+`") }`, nil, "CONFLICT") // paused agents don't run
	a.ok("operatorA", `mutation { resumeAgent(agentId:"`+id+`") { status } }`, nil)
	d = a.ok("operatorA", `mutation { triggerManualHeartbeat(agentId:"`+id+`") }`, nil)
	if d["triggerManualHeartbeat"] != true {
		t.Errorf("heartbeat: %v", d)
	}
}

func TestPolicyCompileInstallsWorkspaceScopedRule(t *testing.T) {
	a := newAPI(t)
	d := a.ok("ownerA", `mutation { compileAndSavePolicy(input:{workspaceId:"`+a.wsA+`", rawPolicyText:"refund above 200"}) { triggerEvent assignedApprovalRole compiledAstJson } }`, nil)
	if d["compileAndSavePolicy"].(map[string]any)["assignedApprovalRole"] != "Finance" {
		t.Errorf("policy: %v", d)
	}
	// 500 is under the default 1000 limit, but A's new rule (200) must now hold it...
	if r := a.policy.EvaluateActionFor(a.wsA, "order.refund_requested", `{"refund_amount":"500"}`); r.Decision != kernel.DecisionSuspendApproval {
		t.Errorf("workspace A must enforce its compiled policy: %+v", r)
	}
	// ...and B must be unaffected.
	if r := a.policy.EvaluateActionFor(a.wsB, "order.refund_requested", `{"refund_amount":"500"}`); r.Decision != kernel.DecisionAllow {
		t.Errorf("workspace B must not inherit A's policy: %+v", r)
	}
	rules, _ := a.repo.ListEnabledPolicyRules(context.Background())
	if len(rules) != 1 || rules[0].WorkspaceID != a.wsA {
		t.Errorf("policy must be persisted for A: %+v", rules)
	}
	// Cross-workspace compile is forbidden.
	a.fail("ownerA", `mutation { compileAndSavePolicy(input:{workspaceId:"`+a.wsB+`", rawPolicyText:"refund above 1"}) { id } }`, nil, "FORBIDDEN")
}

func TestGoalsPlanAndExecution(t *testing.T) {
	a := newAPI(t)
	deadline := time.Now().Add(21 * 24 * time.Hour).UTC().Format(time.RFC3339)
	a.fail("operatorA", `mutation { createGoal(input:{workspaceId:"`+a.wsA+`", title:"x", description:"d", targetDeadline:"2001-01-01T00:00:00Z", constraints:{}}) { id } }`, nil, "BAD_USER_INPUT")
	d := a.ok("operatorA", `mutation { createGoal(input:{workspaceId:"`+a.wsA+`", title:"Clear stock", description:"21 days", targetDeadline:"`+deadline+`", constraints:{min_margin:"25%"}}) { id status projectedMarginPct milestones { stepOrder action } } }`, nil)
	g := d["createGoal"].(map[string]any)
	if g["status"] != "DRAFT" || len(g["milestones"].([]any)) != 2 || g["projectedMarginPct"] != "26.4" {
		t.Fatalf("goal: %v", g)
	}
	id := g["id"].(string)

	run := func() {
		d := a.ok("operatorA", `mutation { triggerPlanExecution(goalId:"`+id+`") { status } }`, nil)
		if d["triggerPlanExecution"].(map[string]any)["status"] != "ACTIVE" {
			t.Errorf("goal not active: %v", d)
		}
	}
	run()
	run() // idempotent: must not create duplicate tickets
	ts, _ := a.repo.ListTickets(context.Background(), a.wsA, nil, 50, 0)
	if len(ts) != 2 {
		t.Errorf("two milestones => exactly two tickets even when triggered twice, got %d", len(ts))
	}
	if a.exec.calls != 2 {
		t.Errorf("each milestone executes once, got %d", a.exec.calls)
	}
	a.fail("ownerB", `{ goal(id:"`+id+`") { id } }`, nil, "NOT_FOUND")
}

func TestMCPRegistrationVaultAndHealth(t *testing.T) {
	a := newAPI(t)
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))
	}))
	defer mcpSrv.Close()

	reg := func(token, name, transport, uri, creds, scopes string) (int, gqlResp) {
		return a.do(token, `mutation($i: RegisterMCPInput!) { registerMCPConnection(input:$i) { id serverName healthStatus allowedScopes } }`,
			map[string]any{"i": map[string]any{"workspaceId": a.wsA, "serverName": name, "transportType": transport, "serverUri": uri, "rawCredentialsJson": creds, "allowedScopes": strings.Split(scopes, ",")}})
	}
	if _, r := reg("operatorA", "shopify", "http", mcpSrv.URL, `{"bearer_token":"x"}`, "shopify.*"); len(r.Errors) == 0 || r.Errors[0].Extensions["code"] != "FORBIDDEN" {
		t.Errorf("operators cannot register connections: %+v", r.Errors)
	}
	for name, args := range map[string][]string{
		"stdio":        {"shopify", "stdio", "npx evil", `{"bearer_token":"x"}`, "shopify.*"},
		"bad name":     {"Shopify!", "http", mcpSrv.URL, `{"bearer_token":"x"}`, "shopify.*"},
		"unknown cred": {"shopify", "http", mcpSrv.URL, `{"bearer":"x"}`, "shopify.*"},
		"bad creds":    {"shopify", "http", mcpSrv.URL, `nope`, "shopify.*"},
	} {
		if _, r := reg("ownerA", args[0], args[1], args[2], args[3], args[4]); len(r.Errors) == 0 || r.Errors[0].Extensions["code"] != "BAD_USER_INPUT" {
			t.Errorf("%s must be rejected: %+v", name, r.Errors)
		}
	}

	code, r := reg("ownerA", "shopify", "http", mcpSrv.URL, `{"bearer_token":"super-secret-token"}`, "shopify.get_order,shopify.*")
	if code != 200 || len(r.Errors) > 0 {
		t.Fatalf("register: %+v", r.Errors)
	}
	conn := r.Data["registerMCPConnection"].(map[string]any)
	if b, _ := json.Marshal(r); strings.Contains(string(b), "super-secret-token") {
		t.Error("credentials must never be returned")
	}
	// The secret is encrypted at rest.
	stored, _ := a.repo.GetMCPConnection(context.Background(), a.wsA, conn["id"].(string))
	if strings.Contains(stored.EncryptedAuthRef, "super-secret-token") || !strings.HasPrefix(stored.EncryptedAuthRef, "v1:") {
		t.Errorf("credentials must be stored encrypted: %q", stored.EncryptedAuthRef)
	}

	d := a.ok("operatorA", `mutation { testMCPHealth(connectionId:"`+conn["id"].(string)+`") }`, nil)
	if d["testMCPHealth"] != "HEALTHY" {
		t.Errorf("health: %v", d)
	}
	d = a.ok("auditorA", `{ mcpConnections(workspaceId:"`+a.wsA+`") { serverName healthStatus } }`, nil)
	if len(d["mcpConnections"].([]any)) != 1 {
		t.Errorf("connections: %v", d)
	}
	a.fail("ownerB", `mutation { testMCPHealth(connectionId:"`+conn["id"].(string)+`") }`, nil, "NOT_FOUND")
}

func TestErrorsAreScrubbedAndLimitsEnforced(t *testing.T) {
	a := newAPI(t)
	// A malformed id can match nothing: NOT_FOUND, not a database error leaking out.
	a.fail("ownerA", `{ ticket(id:"not-a-uuid") { id } }`, nil, "NOT_FOUND")
	// Unexpected errors are hidden from clients.
	if ge := gql.PresentError(context.Background(), errors.New("pq: relation secret_table does not exist")); ge.Message != "internal error" {
		t.Errorf("internal errors must be scrubbed, got %q", ge.Message)
	}
	// Introspection is disabled outside dev.
	if _, r := a.do("ownerA", `{ __schema { types { name } } }`, nil); len(r.Errors) == 0 {
		t.Error("introspection must be off")
	}
	// Complexity limit (200): a query with a very wide selection is rejected.
	var b strings.Builder
	b.WriteString(`{ `)
	for i := 0; i < 120; i++ {
		b.WriteString(`t` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `: tickets(workspaceId:"` + a.wsA + `") { id status } `)
	}
	b.WriteString(`}`)
	if _, r := a.do("ownerA", b.String(), nil); len(r.Errors) == 0 {
		t.Error("an over-complex query must be rejected")
	}
}

func TestSubscriptionsOverAuthenticatedWebsocket(t *testing.T) {
	a := newAPI(t)
	wsURL := "ws" + strings.TrimPrefix(a.ts.URL, "http") + "/graphql"
	dialer := websocket.Dialer{Subprotocols: []string{"graphql-transport-ws"}, HandshakeTimeout: 5 * time.Second}

	connect := func(init map[string]any) (*websocket.Conn, string) {
		c, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.WriteJSON(map[string]any{"type": "connection_init", "payload": init})
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var m struct{ Type string }
		_ = c.ReadJSON(&m)
		return c, m.Type
	}

	// No credentials / bad ticket: refused.
	if c, typ := connect(map[string]any{}); typ == "connection_ack" {
		t.Error("anonymous websocket must be refused")
	} else {
		c.Close()
	}
	if c, typ := connect(map[string]any{"ticket": "bogus"}); typ == "connection_ack" {
		t.Error("bogus ticket must be refused")
	} else {
		c.Close()
	}

	p, _ := a.svc.Authenticate(context.Background(), a.tokens["operatorA"])
	ticket, _ := a.svc.IssueWSTicket(p)
	c, typ := connect(map[string]any{"ticket": ticket})
	if typ != "connection_ack" {
		t.Fatalf("valid ticket must be accepted, got %q", typ)
	}
	defer c.Close()
	// Tickets are single-use.
	if c2, typ := connect(map[string]any{"ticket": ticket}); typ == "connection_ack" {
		t.Error("a ticket must not be reusable")
	} else {
		c2.Close()
	}

	// Subscribe to the caller's workspace; subscribing to another workspace errors.
	_ = c.WriteJSON(map[string]any{"id": "other", "type": "subscribe", "payload": map[string]any{"query": `subscription { ticketUpdated(workspaceId:"` + a.wsB + `") { id } }`}})
	_ = c.WriteJSON(map[string]any{"id": "mine", "type": "subscribe", "payload": map[string]any{"query": `subscription { ticketUpdated(workspaceId:"` + a.wsA + `") { id status actionType } }`}})
	time.Sleep(300 * time.Millisecond) // let the subscription attach

	var seeded sync.WaitGroup
	seeded.Add(1)
	go func() {
		defer seeded.Done()
		a.seedRefund("sub", "5000")
	}()
	defer seeded.Wait()

	gotForbidden, gotEvent := false, false
	for i := 0; i < 10 && !(gotForbidden && gotEvent); i++ {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var m struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := c.ReadJSON(&m); err != nil {
			t.Fatalf("read: %v (forbidden=%v event=%v)", err, gotForbidden, gotEvent)
		}
		switch {
		case m.ID == "other":
			// A refused subscription arrives as an error payload and must carry no data.
			if strings.Contains(string(m.Payload), "FORBIDDEN") {
				gotForbidden = true
			} else if strings.Contains(string(m.Payload), "order.refund_requested") {
				t.Fatal("must never receive another workspace's events")
			}
		case m.ID == "mine" && m.Type == "next":
			if strings.Contains(string(m.Payload), "order.refund_requested") {
				gotEvent = true
			}
		}
	}
	if !gotForbidden || !gotEvent {
		t.Errorf("expected forbidden for B's workspace and a live event for A's (forbidden=%v event=%v)", gotForbidden, gotEvent)
	}
}

var _ = errors.New
