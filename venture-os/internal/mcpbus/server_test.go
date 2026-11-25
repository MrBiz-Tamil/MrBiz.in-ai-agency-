package mcpbus_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	approvalv1 "github.com/mrbiz/venture-os/gen/venture/approval/v1"
	"github.com/mrbiz/venture-os/internal/mcpbus"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/ratelimit"
)

var signer, _ = mcptoken.NewSigner([]byte("0123456789abcdef0123456789abcdef"))

// stubAgent satisfies the generated client interface; embedding it leaves unused methods nil (they panic if a
// test unexpectedly reaches them, which is the point).
type stubAgent struct {
	agentv1connect.AgentServiceClient
	status  func(*connect.Request[agentv1.GetApprovalStatusRequest]) (*connect.Response[agentv1.GetApprovalStatusResponse], error)
	gotAuth string
	calls   int
}

func (s *stubAgent) GetApprovalStatus(_ context.Context, r *connect.Request[agentv1.GetApprovalStatusRequest]) (*connect.Response[agentv1.GetApprovalStatusResponse], error) {
	s.calls++
	s.gotAuth = r.Header().Get("Authorization")
	return s.status(r)
}

type harness struct {
	srv   *httptest.Server
	agent *stubAgent
	token string
	tid   uuid.UUID
}

func newHarness(t *testing.T, mutate func(*mcpbus.Server)) *harness {
	t.Helper()
	h := &harness{agent: &stubAgent{}, tid: uuid.New()}
	h.token, _ = signer.Mint(h.tid, "sales", time.Hour)
	s := mcpbus.Server{Agent: h.agent, Signer: signer, Version: "test"}
	if mutate != nil {
		mutate(&s)
	}
	h.srv = httptest.NewServer(mcpbus.New(s))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) post(t *testing.T, token, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

type rpc struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *harness) rpc(t *testing.T, method string, params any) rpc {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, raw := h.post(t, h.token, string(body), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%s: HTTP %d: %s", method, resp.StatusCode, raw)
	}
	var r rpc
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("%s: bad JSON-RPC response %q: %v", method, raw, err)
	}
	return r
}

type toolOut struct {
	Content []struct{ Type, Text string } `json:"content"`
	IsError bool                          `json:"isError"`
}

func (h *harness) call(t *testing.T, tool string, args any) toolOut {
	t.Helper()
	r := h.rpc(t, "tools/call", map[string]any{"name": tool, "arguments": args})
	if r.Error != nil {
		t.Fatalf("%s: protocol error %+v", tool, r.Error)
	}
	var out toolOut
	if err := json.Unmarshal(r.Result, &out); err != nil || len(out.Content) != 1 || out.Content[0].Type != "text" {
		t.Fatalf("%s: bad tool result %s", tool, r.Result)
	}
	return out
}

func TestInitializeNegotiatesVersionAndAdvertisesTools(t *testing.T) {
	h := newHarness(t, nil)
	for _, tc := range []struct{ ask, want string }{
		{"2025-03-26", "2025-03-26"}, {"2024-11-05", "2024-11-05"}, {"2025-06-18", "2025-06-18"}, {"2099-01-01", "2025-06-18"}, {"", "2025-06-18"},
	} {
		r := h.rpc(t, "initialize", map[string]any{"protocolVersion": tc.ask, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "hermes", "version": "1"}})
		var res struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      struct{ Name, Version string }
			Instructions    string
		}
		_ = json.Unmarshal(r.Result, &res)
		if res.ProtocolVersion != tc.want || res.Capabilities["tools"] == nil || res.ServerInfo.Name != "venture-os-mcpbus" || res.Instructions == "" {
			t.Errorf("ask %q: %+v", tc.ask, res)
		}
	}
	if r := h.rpc(t, "ping", nil); r.Error != nil || string(r.Result) != "{}" {
		t.Errorf("ping: %+v", r)
	}
	// A notification gets 202 and no body.
	resp, body := h.post(t, h.token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if resp.StatusCode != http.StatusAccepted || len(body) != 0 {
		t.Errorf("notification: %d %q", resp.StatusCode, body)
	}
}

func TestToolListIsCompleteTypedAndHasNoTenantParameter(t *testing.T) {
	h := newHarness(t, nil)
	r := h.rpc(t, "tools/list", nil)
	var res struct {
		Tools []struct {
			Name        string
			Description string
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any
		}
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	want := []string{"list_pricing_rules", "calculate_price", "forecast_cash_flow", "create_quote", "draft_invoice",
		"draft_purchase_order", "request_human_approval", "escalate_blocker", "get_approval_status"}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
		if tl.Description == "" || tl.InputSchema["type"] != "object" || tl.InputSchema["additionalProperties"] != false {
			t.Errorf("%s: schema must be a closed object with a description: %+v", tl.Name, tl.InputSchema)
		}
		props, _ := tl.InputSchema["properties"].(map[string]any)
		for _, forbidden := range []string{"tenant_id", "tenant", "business_id", "organization_id"} {
			if _, bad := props[forbidden]; bad {
				t.Errorf("%s exposes a %q parameter: tenancy must come from the token alone", tl.Name, forbidden)
			}
		}
		for _, req := range toStrings(tl.InputSchema["required"]) {
			if _, ok := props[req]; !ok {
				t.Errorf("%s requires %q which is not a property", tl.Name, req)
			}
		}
		// Every document-creating tool must demand an idempotency key.
		if strings.HasPrefix(tl.Name, "draft_") || tl.Name == "create_quote" || tl.Name == "request_human_approval" || tl.Name == "escalate_blocker" {
			if !contains(toStrings(tl.InputSchema["required"]), "idempotency_key") {
				t.Errorf("%s does not require idempotency_key", tl.Name)
			}
		}
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("tool %s missing", w)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("got %d tools, want %d", len(res.Tools), len(want))
	}
}

func toStrings(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			out = append(out, x.(string))
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestTransportRules(t *testing.T) {
	h := newHarness(t, nil)
	valid := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	if resp, _ := h.post(t, "", valid, nil); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if resp, _ := h.post(t, "forged.token.value", valid, nil); resp.StatusCode != 401 {
		t.Errorf("forged token: %d", resp.StatusCode)
	}
	if resp, _ := h.post(t, h.token, valid, map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Errorf("foreign Origin: %d", resp.StatusCode)
	}
	if resp, _ := h.post(t, h.token, valid, map[string]string{"Content-Type": "text/plain"}); resp.StatusCode != 415 {
		t.Errorf("wrong content type: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
		t.Errorf("GET: %d", resp.StatusCode)
	}
	if resp, _ := h.post(t, h.token, strings.Repeat("x", 2<<20), nil); resp.StatusCode != 413 {
		t.Errorf("oversize body: %d", resp.StatusCode)
	}

	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"parse error":    {`{not json`, -32700},
		"batch":          {`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, -32600},
		"wrong version":  {`{"jsonrpc":"1.0","id":1,"method":"ping"}`, -32600},
		"unknown method": {`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, -32601},
		"bad params":     {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, -32602},
		"unknown tool":   {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"drop_database"}}`, -32602},
	} {
		resp, raw := h.post(t, h.token, tc.body, nil)
		var r rpc
		_ = json.Unmarshal(raw, &r)
		if resp.StatusCode != 200 || r.Error == nil || r.Error.Code != tc.code {
			t.Errorf("%s: HTTP %d %s", name, resp.StatusCode, raw)
		}
	}
}

func TestToolArgumentsAreStrictAndTenantCannotBeSmuggled(t *testing.T) {
	h := newHarness(t, nil)
	h.agent.status = func(*connect.Request[agentv1.GetApprovalStatusRequest]) (*connect.Response[agentv1.GetApprovalStatusResponse], error) {
		t.Fatal("the control plane must not be reached with invalid arguments")
		return nil, nil
	}
	for name, args := range map[string]any{
		"tenant smuggled": map[string]any{"approval_id": uuid.NewString(), "tenant_id": uuid.NewString()},
		"unknown field":   map[string]any{"approval_id": "x", "extra": 1},
		"wrong type":      map[string]any{"approval_id": 12},
	} {
		out := h.call(t, "get_approval_status", args)
		if !out.IsError || !strings.Contains(out.Content[0].Text, "Invalid arguments") {
			t.Errorf("%s: %+v", name, out)
		}
	}
	if h.agent.calls != 0 {
		t.Fatalf("control plane reached %d times", h.agent.calls)
	}
}

func TestTokenIsForwardedAndErrorsAreMappedSafely(t *testing.T) {
	h := newHarness(t, nil)
	id := uuid.NewString()
	h.agent.status = func(r *connect.Request[agentv1.GetApprovalStatusRequest]) (*connect.Response[agentv1.GetApprovalStatusResponse], error) {
		return connect.NewResponse(&agentv1.GetApprovalStatusResponse{Approval: &agentv1.ApprovalState{ApprovalId: r.Msg.ApprovalId, Status: "REJECTED", DecisionNote: "too cheap"}}), nil
	}
	out := h.call(t, "get_approval_status", map[string]any{"approval_id": id})
	if out.IsError || !strings.Contains(out.Content[0].Text, "REJECTED") || !strings.Contains(out.Content[0].Text, "too cheap") {
		t.Fatalf("ok path: %+v", out)
	}
	if h.agent.gotAuth != "Bearer "+h.token {
		t.Fatalf("the caller's token must be forwarded verbatim, got %q", h.agent.gotAuth)
	}

	for name, tc := range map[string]struct {
		err      error
		contains string
		leaks    string
	}{
		"not found":      {connect.NewError(connect.CodeNotFound, errors.New("approval not found")), "approval not found", ""},
		"precondition":   {connect.NewError(connect.CodeFailedPrecondition, errors.New("the business is draft, not ACTIVE")), "not ACTIVE", ""},
		"invalid":        {connect.NewError(connect.CodeInvalidArgument, errors.New("unknown tax_code \"X\"")), "unknown tax_code", ""},
		"internal":       {connect.NewError(connect.CodeInternal, errors.New("pq: password=hunter2 relation x does not exist")), "problem", "hunter2"},
		"unavailable":    {connect.NewError(connect.CodeUnavailable, errors.New("dial tcp 10.0.0.5:5432: refused")), "problem", "10.0.0.5"},
		"unauthed":       {connect.NewError(connect.CodeUnauthenticated, errors.New("token expired at 12:00")), "credentials", "12:00"},
		"plain go error": {errors.New("boom with secrets"), "problem", "secrets"},
	} {
		h.agent.status = func(*connect.Request[agentv1.GetApprovalStatusRequest]) (*connect.Response[agentv1.GetApprovalStatusResponse], error) {
			return nil, tc.err
		}
		o := h.call(t, "get_approval_status", map[string]any{"approval_id": id})
		text := o.Content[0].Text
		if !o.IsError || !strings.Contains(text, tc.contains) || (tc.leaks != "" && strings.Contains(text, tc.leaks)) {
			t.Errorf("%s: %+v", name, o)
		}
	}
}

func TestRateLimitingIsPerTenantAndFailsClosed(t *testing.T) {
	lim := &ratelimit.Memory{Limit: 2, Window: time.Minute}
	h := newHarness(t, func(s *mcpbus.Server) { s.Limiter = lim })
	h.agent.status = func(*connect.Request[agentv1.GetApprovalStatusRequest]) (*connect.Response[agentv1.GetApprovalStatusResponse], error) {
		return connect.NewResponse(&agentv1.GetApprovalStatusResponse{Approval: &agentv1.ApprovalState{Status: "PENDING_APPROVAL"}}), nil
	}
	args := map[string]any{"approval_id": uuid.NewString()}
	for i := 0; i < 2; i++ {
		if o := h.call(t, "get_approval_status", args); o.IsError {
			t.Fatalf("call %d limited too early: %+v", i+1, o)
		}
	}
	if o := h.call(t, "get_approval_status", args); !o.IsError || !strings.Contains(o.Content[0].Text, "Rate limit") {
		t.Fatalf("3rd call not limited: %+v", o)
	}
	// Another business's token has its own budget.
	other, _ := signer.Mint(uuid.New(), "", time.Hour)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_approval_status","arguments":{"approval_id":"` + uuid.NewString() + `"}}}`
	_, raw := h.post(t, other, body, nil)
	if strings.Contains(string(raw), "Rate limit") {
		t.Fatalf("tenant B was limited by tenant A: %s", raw)
	}
	// Non-tool methods are never throttled.
	if r := h.rpc(t, "tools/list", nil); r.Error != nil {
		t.Fatalf("tools/list throttled: %+v", r.Error)
	}
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, uuid.UUID) (bool, time.Duration, error) {
	return false, 0, errors.New("redis down")
}

func TestLimiterOutagePolicy(t *testing.T) {
	ok := func(*connect.Request[agentv1.GetApprovalStatusRequest]) (*connect.Response[agentv1.GetApprovalStatusResponse], error) {
		return connect.NewResponse(&agentv1.GetApprovalStatusResponse{Approval: &agentv1.ApprovalState{Status: "PENDING_APPROVAL"}}), nil
	}
	closed := newHarness(t, func(s *mcpbus.Server) { s.Limiter = brokenLimiter{} })
	closed.agent.status = ok
	if o := closed.call(t, "get_approval_status", map[string]any{"approval_id": "x"}); !o.IsError || closed.agent.calls != 0 {
		t.Errorf("default must fail closed: %+v calls=%d", o, closed.agent.calls)
	}
	open := newHarness(t, func(s *mcpbus.Server) { s.Limiter = brokenLimiter{}; s.LimiterFailOpen = true })
	open.agent.status = ok
	if o := open.call(t, "get_approval_status", map[string]any{"approval_id": "x"}); o.IsError || open.agent.calls != 1 {
		t.Errorf("fail-open policy: %+v calls=%d", o, open.agent.calls)
	}
}

func TestAllowedOriginPasses(t *testing.T) {
	h := newHarness(t, func(s *mcpbus.Server) { s.AllowedOrigins = []string{"https://app.example"} })
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	resp, _ := h.post(t, h.token, string(body), map[string]string{"Origin": "https://app.example"})
	if resp.StatusCode != 200 {
		t.Fatalf("allowlisted origin: %d", resp.StatusCode)
	}
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func bearerOIDC(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(c, req)
		}
	})
}

func approvalListReq() *approvalv1.ListApprovalsRequest { return &approvalv1.ListApprovalsRequest{} }
