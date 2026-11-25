// Package mcpbus is the tenant-scoped MCP (Model Context Protocol) server agents use to act on a business.
//
// It speaks JSON-RPC 2.0 over MCP's streamable HTTP transport (stateless, JSON responses) and is deliberately thin:
//
//   - It authenticates every request with a per-tenant bearer token (internal/mcptoken). The tenant is whatever
//     the token says; no tool has a tenant parameter, and unknown arguments are rejected, so a prompt-injected
//     agent cannot name another business.
//   - It holds no database credentials. Every tool is a typed call to the control plane's AgentService, which
//     re-verifies the same token. All arithmetic and state transitions happen there, in deterministic code.
//   - Anything outward-facing becomes a PENDING_APPROVAL item for the founder; the tool result says so plainly.
package mcpbus

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	"github.com/mrbiz/venture-os/internal/bearer"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/ratelimit"
)

// Supported MCP protocol revisions, newest first.
var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const (
	maxBodyBytes = 1 << 20
	callTimeout  = 30 * time.Second
	serverName   = "venture-os-mcpbus"
)

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// Server is the HTTP handler for POST /mcp.
type Server struct {
	Agent   agentv1connect.AgentServiceClient
	Signer  *mcptoken.Signer
	Limiter ratelimit.Limiter // nil disables rate limiting
	Log     *slog.Logger
	Version string
	// AllowedOrigins lists browser Origins permitted to call the bus. Agents are not browsers and send no
	// Origin header; any request that does carry one is refused unless listed (DNS-rebinding protection).
	AllowedOrigins []string
	// LimiterFailOpen lets calls through when the limiter itself errors (e.g. Redis down). Default: closed.
	LimiterFailOpen bool
	// EnableCRM offers the crm_ops tools. Set it only when the control plane has a real CRM (VENTURE_CONNECTOR_CRM=real):
	// with a simulated CRM there is no data behind them, and tools that cannot work are left out, not faked.
	EnableCRM bool
	// EnableCollab offers the meetings_ops and notes_ops tools (calendar, Google Meet, Zoom, Notion). Set it when the
	// deployment has integrations configured; each business still has to connect its own accounts.
	EnableCollab bool

	tools []tool
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// New returns a Server with the standard tool set registered.
func New(s Server) *Server {
	s.tools = standardTools()
	if s.EnableCRM {
		s.tools = append(s.tools, crmTools()...)
	}
	if s.EnableCollab {
		s.tools = append(s.tools, collabTools()...)
	}
	if s.Version == "" {
		s.Version = "dev"
	}
	return &s
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// ServeHTTP implements http.Handler for the /mcp endpoint.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(s.AllowedOrigins, origin) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		// No server-initiated stream and no sessions: GET/DELETE are not offered.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this MCP server only accepts POST", http.StatusMethodNotAllowed)
		return
	}
	token, ok := bearer.Token(r.Header.Get("Authorization"))
	if !ok {
		unauthorized(w)
		return
	}
	claims, err := s.Signer.Verify(token)
	if err != nil {
		unauthorized(w)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		s.reply(w, nil, nil, &rpcError{codeInvalidRequest, "JSON-RPC batching is not supported"})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.reply(w, nil, nil, &rpcError{codeParse, "parse error"})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.reply(w, req.ID, nil, &rpcError{codeInvalidRequest, "not a valid JSON-RPC 2.0 request"})
		return
	}
	// Notifications carry no id and get no body.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		s.initialize(w, req)
	case "ping":
		s.reply(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		s.listTools(w, req)
	case "tools/call":
		s.callTool(w, r.Context(), req, token, claims)
	default:
		s.reply(w, req.ID, nil, &rpcError{codeMethodNotFound, "method not found: " + req.Method})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="venture-os"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (s *Server) reply(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
}

func (s *Server) initialize(w http.ResponseWriter, req rpcRequest) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &p)
	version := supportedVersions[0]
	if slices.Contains(supportedVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	s.reply(w, req.ID, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": serverName, "version": s.Version},
		"instructions": "Tools act on exactly one business, fixed by your credentials. All money figures come from these tools; " +
			"never calculate them yourself. Anything outward-facing is queued for the founder's approval and is not done until approved.",
	}, nil)
}

func (s *Server) listTools(w http.ResponseWriter, req rpcRequest) {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]any{
			"name": t.Name, "description": t.Description, "inputSchema": json.RawMessage(t.Schema),
			"annotations": map[string]any{"readOnlyHint": t.ReadOnly, "destructiveHint": false, "openWorldHint": false},
		})
	}
	s.reply(w, req.ID, map[string]any{"tools": out}, nil)
}

type toolResult struct {
	Content []map[string]string `json:"content"`
	IsError bool                `json:"isError,omitempty"`
}

func textResult(text string, isErr bool) toolResult {
	return toolResult{Content: []map[string]string{{"type": "text", "text": text}}, IsError: isErr}
}

func (s *Server) callTool(w http.ResponseWriter, ctx context.Context, req rpcRequest, token string, claims mcptoken.Claims) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.reply(w, req.ID, nil, &rpcError{codeInvalidParams, "params must be {name, arguments}"})
		return
	}
	idx := slices.IndexFunc(s.tools, func(t tool) bool { return t.Name == p.Name })
	if idx < 0 {
		s.reply(w, req.ID, nil, &rpcError{codeInvalidParams, "unknown tool: " + p.Name})
		return
	}
	t := s.tools[idx]
	start := time.Now()

	if s.Limiter != nil {
		ok, retry, err := s.Limiter.Allow(ctx, claims.TenantID)
		switch {
		case err != nil && !s.LimiterFailOpen:
			s.log().Error("rate limiter unavailable; refusing call", "tenant", claims.TenantID, "tool", t.Name, "err", err)
			s.reply(w, req.ID, textResult("The tool service is temporarily unavailable. Please retry shortly.", true), nil)
			return
		case err != nil:
			s.log().Error("rate limiter unavailable; allowing call", "tenant", claims.TenantID, "tool", t.Name, "err", err)
		case !ok:
			s.log().Warn("rate limited", "tenant", claims.TenantID, "tool", t.Name)
			s.reply(w, req.ID, textResult("Rate limit exceeded for this business. Wait "+retry.Round(time.Second).String()+" before calling tools again.", true), nil)
			return
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	text, err := t.Call(callCtx, &call{agent: s.Agent, token: token}, args)
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	// Log who called what and how it went; never the arguments or results, which are business data.
	s.log().Info("tool call", "tenant", claims.TenantID, "tool", t.Name, "outcome", outcome, "ms", time.Since(start).Milliseconds())
	if err != nil {
		s.reply(w, req.ID, textResult(userMessage(err), true), nil)
		return
	}
	s.reply(w, req.ID, textResult(text, false), nil)
}

// call carries what a tool needs to reach the control plane on the caller's behalf.
type call struct {
	agent agentv1connect.AgentServiceClient
	token string
}
