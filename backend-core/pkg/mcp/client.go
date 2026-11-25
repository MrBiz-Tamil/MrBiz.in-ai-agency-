// Package mcp is a minimal Model Context Protocol client (Streamable HTTP transport) and the
// gateway that routes semantic tool names to the workspace's registered MCP servers.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	maxResponseBytes = 1 << 20
	callTimeout      = 20 * time.Second
	protocolVersion  = "2025-03-26"
)

// Credentials are what the vault stores for a connection. They are only ever decrypted
// inside the gateway and attached to outbound requests; models and the UI never see them.
type Credentials struct {
	BearerToken string            `json:"bearer_token,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// Server identifies one MCP endpoint plus its credentials.
type Server struct {
	URI   string
	Creds Credentials
}

// ValidateServerURI rejects URLs that could be used to reach internal services (SSRF).
// With allowPrivate (development), loopback/private addresses and plain http are accepted.
func ValidateServerURI(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("server URI is not a valid URL")
	}
	if u.User != nil {
		return errors.New("server URI must not contain credentials")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowPrivate {
			return errors.New("server URI must use https")
		}
	default:
		return errors.New("server URI must be an http(s) URL (stdio transport is not supported)")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !allowPrivate && isBlockedIP(ip) {
		return errors.New("server URI points at a private or reserved address")
	}
	if !allowPrivate {
		h := strings.ToLower(u.Hostname())
		if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".local") {
			return errors.New("server URI points at a private host name")
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.ParseIP("169.254.169.254"))
}

// NewHTTPClient builds an HTTP client that re-checks the resolved address at connect time
// (defeating DNS rebinding) and refuses redirects.
func NewHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
				return fmt.Errorf("connection to %s is blocked", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:       callTimeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext, ResponseHeaderTimeout: callTimeout, MaxIdleConns: 20, IdleConnTimeout: 60 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type Client struct {
	HTTP *http.Client
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ErrHTTPStatus carries a non-2xx status from the MCP server.
type ErrHTTPStatus struct{ Code int }

func (e ErrHTTPStatus) Error() string { return fmt.Sprintf("MCP server returned HTTP %d", e.Code) }

func (c *Client) rpc(ctx context.Context, srv Server, id int, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URI, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	for k, v := range srv.Creds.Headers {
		req.Header.Set(k, v)
	}
	if srv.Creds.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+srv.Creds.BearerToken)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, ErrHTTPStatus{Code: resp.StatusCode}
	}

	var payload []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		payload, err = firstSSEData(io.LimitReader(resp.Body, maxResponseBytes), id)
	} else {
		payload, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if err == nil && len(payload) > maxResponseBytes {
			err = errors.New("MCP response too large")
		}
	}
	if err != nil {
		return nil, err
	}

	var out rpcResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, errors.New("MCP server returned invalid JSON")
	}
	if out.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

// firstSSEData returns the JSON-RPC message in the event stream whose id matches.
func firstSSEData(r io.Reader, id int) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxResponseBytes)
	var data []string
	flush := func() ([]byte, bool) {
		if len(data) == 0 {
			return nil, false
		}
		msg := []byte(strings.Join(data, "\n"))
		data = nil
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(msg, &probe) == nil && string(probe.ID) == fmt.Sprint(id) {
			return msg, true
		}
		return nil, false
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if msg, ok := flush(); ok {
				return msg, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if msg, ok := flush(); ok {
		return msg, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("MCP event stream ended without a response")
}

// Ping performs the MCP initialize handshake.
func (c *Client) Ping(ctx context.Context, srv Server) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.rpc(ctx, srv, 1, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "abos-gateway", "version": "1"},
	})
	return err
}

// CallTool invokes a tool and returns its result as JSON. A tool-level error (isError)
// becomes a Go error so the ticket is marked FAILED rather than falsely COMPLETED.
func (c *Client) CallTool(ctx context.Context, srv Server, tool string, args json.RawMessage) (string, error) {
	var arguments any = map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return "", errors.New("tool arguments are not valid JSON")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.rpc(ctx, srv, 2, "tools/call", map[string]any{"name": tool, "arguments": arguments})
	if err != nil {
		return "", err
	}
	var parsed struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(res, &parsed) == nil && parsed.IsError {
		msg := "tool reported an error"
		for _, c := range parsed.Content {
			if c.Type == "text" && c.Text != "" {
				msg = c.Text
				break
			}
		}
		return "", errors.New(msg)
	}
	return string(res), nil
}
