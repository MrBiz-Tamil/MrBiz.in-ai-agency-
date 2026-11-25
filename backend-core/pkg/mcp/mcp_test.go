package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/vault"
)

type rpcReq struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// fakeMCP is a minimal MCP server. mode: "json" or "sse".
func fakeMCP(t *testing.T, mode string, seen *http.Header, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.Header.Clone()
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		var req rpcReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "boom" {
				result = map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "out of stock"}}}
			} else {
				result = map[string]any{"content": []map[string]string{{"type": "text", "text": fmt.Sprintf("%s:%v", p.Name, p.Arguments["order_id"])}}}
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "no such method"}})
			return
		}
		msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if mode == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			// a notification first (different id), then the real response
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(msg)
	}))
}

func dev() *Client { return &Client{HTTP: NewHTTPClient(true)} }

func TestClient_CallToolJSONAndSSE(t *testing.T) {
	for _, mode := range []string{"json", "sse"} {
		var hdr http.Header
		srv := fakeMCP(t, mode, &hdr, 0)
		out, err := dev().CallTool(context.Background(), Server{URI: srv.URL, Creds: Credentials{BearerToken: "tok", Headers: map[string]string{"X-Api-Key": "k"}}}, "get_order", json.RawMessage(`{"order_id":"o1"}`))
		srv.Close()
		if err != nil || !strings.Contains(out, "get_order:o1") {
			t.Fatalf("%s: %q %v", mode, out, err)
		}
		if hdr.Get("Authorization") != "Bearer tok" || hdr.Get("X-Api-Key") != "k" {
			t.Errorf("%s: credentials were not injected: %v", mode, hdr)
		}
	}
}

func TestClient_ToolErrorAndHTTPErrors(t *testing.T) {
	srv := fakeMCP(t, "json", nil, 0)
	defer srv.Close()
	_, err := dev().CallTool(context.Background(), Server{URI: srv.URL}, "boom", nil)
	if err == nil || err.Error() != "out of stock" {
		t.Errorf("a tool-level error must surface as an error, got %v", err)
	}

	bad := fakeMCP(t, "json", nil, http.StatusUnauthorized)
	defer bad.Close()
	_, err = dev().CallTool(context.Background(), Server{URI: bad.URL}, "x", nil)
	var hs ErrHTTPStatus
	if !errors.As(err, &hs) || hs.Code != 401 {
		t.Errorf("HTTP 401 must be reported: %v", err)
	}
	if _, err := dev().CallTool(context.Background(), Server{URI: srv.URL}, "x", json.RawMessage(`{bad`)); err == nil {
		t.Error("invalid arguments must be rejected before sending")
	}
}

func TestSSRFProtection(t *testing.T) {
	blocked := []string{"http://example.com/mcp", "https://localhost/mcp", "https://127.0.0.1/mcp", "https://10.0.0.5/mcp",
		"https://169.254.169.254/latest", "https://[::1]/mcp", "https://192.168.1.1/mcp", "https://user:pw@example.com/mcp",
		"ftp://example.com", "stdio://npx", "not a url", "https://db.internal/mcp"}
	for _, u := range blocked {
		if err := ValidateServerURI(u, false); err == nil {
			t.Errorf("%q must be rejected in production", u)
		}
	}
	for _, u := range []string{"https://mcp.example.com/mcp", "https://8.8.8.8/mcp"} {
		if err := ValidateServerURI(u, false); err != nil {
			t.Errorf("%q should be allowed: %v", u, err)
		}
	}
	if err := ValidateServerURI("http://127.0.0.1:9000/mcp", true); err != nil {
		t.Errorf("dev mode allows loopback http: %v", err)
	}

	// Dial-time guard: even if validation were bypassed, a production client refuses loopback.
	srv := fakeMCP(t, "json", nil, 0)
	defer srv.Close()
	prod := &Client{HTTP: NewHTTPClient(false)}
	if _, err := prod.CallTool(context.Background(), Server{URI: srv.URL}, "x", nil); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("production client must refuse to connect to loopback, got %v", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	target := fakeMCP(t, "json", nil, 0)
	defer target.Close()
	redir := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redir.Close()
	if _, err := dev().CallTool(context.Background(), Server{URI: redir.URL}, "x", nil); err == nil {
		t.Error("a redirect must not be followed to another host")
	}
}

type fakeStore struct {
	conns  map[string]*repository.MCPConnection // key: ws/name
	health map[string]string
}

func (f *fakeStore) FindMCPConnectionByName(_ context.Context, ws, name string) (*repository.MCPConnection, error) {
	if c, ok := f.conns[ws+"/"+name]; ok {
		return c, nil
	}
	return nil, repository.ErrNotFound
}
func (f *fakeStore) GetMCPConnection(_ context.Context, ws, id string) (*repository.MCPConnection, error) {
	for k, c := range f.conns {
		if strings.HasPrefix(k, ws+"/") && c.ID == id {
			return c, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (f *fakeStore) SetMCPHealth(_ context.Context, ws, id, h string) error {
	f.health[id] = h
	return nil
}

func gatewayFor(t *testing.T, uri string, scopes string, creds Credentials) (*Gateway, *fakeStore) {
	t.Helper()
	v, _ := vault.NewRandom()
	cj, _ := json.Marshal(creds)
	enc, _ := v.Encrypt("ws1", cj)
	st := &fakeStore{health: map[string]string{}, conns: map[string]*repository.MCPConnection{
		"ws1/shopify": {ID: "c1", WorkspaceID: "ws1", ServerName: "shopify", ServerURI: uri, EncryptedAuthRef: enc, AllowedScopes: json.RawMessage(scopes)},
	}}
	return NewGateway(st, v, dev()), st
}

func TestGateway_RoutesChecksScopesAndInjectsCredentials(t *testing.T) {
	var hdr http.Header
	srv := fakeMCP(t, "json", &hdr, 0)
	defer srv.Close()
	gw, _ := gatewayFor(t, srv.URL, `["shopify.get_order"]`, Credentials{BearerToken: "vault-token"})
	ctx := context.Background()

	out, err := gw.Execute(ctx, "ws1", "shopify.get_order", `{"order_id":"o9"}`)
	if err != nil || !strings.Contains(out, "get_order:o9") {
		t.Fatalf("execute: %q %v", out, err)
	}
	if hdr.Get("Authorization") != "Bearer vault-token" {
		t.Errorf("vault credential must be injected: %v", hdr)
	}
	if _, err := gw.Execute(ctx, "ws1", "shopify.delete_store", `{}`); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("a tool outside allowed scopes must be denied: %v", err)
	}
	if _, err := gw.Execute(ctx, "ws2", "shopify.get_order", `{}`); !errors.Is(err, ErrNoConnection) {
		t.Errorf("another workspace must not reach this connection: %v", err)
	}
	if _, err := gw.Execute(ctx, "ws1", "odoo.get_order", `{}`); !errors.Is(err, ErrNoConnection) {
		t.Errorf("unknown server: %v", err)
	}
	if _, err := gw.Execute(ctx, "ws1", "nodot", `{}`); !errors.Is(err, ErrInvalidToolID) {
		t.Errorf("bad tool id: %v", err)
	}
}

func TestGateway_WildcardScopes(t *testing.T) {
	srv := fakeMCP(t, "json", nil, 0)
	defer srv.Close()
	for _, scopes := range []string{`["shopify.*"]`, `["*"]`} {
		gw, _ := gatewayFor(t, srv.URL, scopes, Credentials{})
		if _, err := gw.Execute(context.Background(), "ws1", "shopify.anything", `{}`); err != nil {
			t.Errorf("scopes %s should allow: %v", scopes, err)
		}
	}
	gw, _ := gatewayFor(t, srv.URL, `[]`, Credentials{})
	if _, err := gw.Execute(context.Background(), "ws1", "shopify.anything", `{}`); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("empty scopes must deny everything: %v", err)
	}
}

func TestGateway_CredentialsBoundToWorkspace(t *testing.T) {
	srv := fakeMCP(t, "json", nil, 0)
	defer srv.Close()
	gw, st := gatewayFor(t, srv.URL, `["*"]`, Credentials{BearerToken: "t"})
	// Row copied into another workspace: ciphertext was bound to ws1 and must not decrypt.
	c := *st.conns["ws1/shopify"]
	c.WorkspaceID = "ws2"
	st.conns["ws2/shopify"] = &c
	if _, err := gw.Execute(context.Background(), "ws2", "shopify.get_order", `{}`); err == nil || !strings.Contains(err.Error(), "cannot be decrypted") {
		t.Errorf("credentials copied across workspaces must not decrypt: %v", err)
	}
}

func TestGateway_Health(t *testing.T) {
	ok := fakeMCP(t, "json", nil, 0)
	defer ok.Close()
	gw, st := gatewayFor(t, ok.URL, `["*"]`, Credentials{})
	if h, err := gw.CheckHealth(context.Background(), "ws1", "c1"); err != nil || h != "HEALTHY" || st.health["c1"] != "HEALTHY" {
		t.Errorf("healthy: %v %v", h, err)
	}
	unauth := fakeMCP(t, "json", nil, http.StatusUnauthorized)
	defer unauth.Close()
	gw, _ = gatewayFor(t, unauth.URL, `["*"]`, Credentials{})
	if h, _ := gw.CheckHealth(context.Background(), "ws1", "c1"); h != "DEGRADED" {
		t.Errorf("reachable but unauthorised must be DEGRADED, got %s", h)
	}
	gw, _ = gatewayFor(t, "http://127.0.0.1:1/mcp", `["*"]`, Credentials{})
	if h, _ := gw.CheckHealth(context.Background(), "ws1", "c1"); h != "CRITICAL" {
		t.Errorf("unreachable must be CRITICAL, got %s", h)
	}
	if _, err := gw.CheckHealth(context.Background(), "ws9", "c1"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("other workspace: %v", err)
	}
}
