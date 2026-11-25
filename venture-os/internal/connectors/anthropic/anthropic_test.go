package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrbiz/venture-os/internal/connectors/anthropic"
	"github.com/mrbiz/venture-os/internal/ports"
)

func server(t *testing.T, status int, body string, capture *map[string]any, hdr *http.Header) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if hdr != nil {
			*hdr = r.Header.Clone()
		}
		if capture != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, capture)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestCompleteSendsExpectedRequestAndParsesReply(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	s := server(t, 200, `{"content":[{"type":"text","text":"hel"},{"type":"text","text":"lo"}],"usage":{"input_tokens":12,"output_tokens":3}}`, &got, &hdr)
	c := &anthropic.Client{APIKey: "sk-test", BaseURL: s.URL, DefaultModel: "claude-sonnet-5-5"}

	resp, err := c.Complete(context.Background(), ports.LLMRequest{Messages: []ports.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "a"},
		{Role: "user", Content: "b"}, // consecutive: must be merged
		{Role: "assistant", Content: "c"},
		{Role: "user", Content: "d"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" || resp.InputTokens != 12 || resp.OutputTokens != 3 {
		t.Fatalf("resp = %+v", resp)
	}
	if hdr.Get("x-api-key") != "sk-test" || hdr.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("headers = %v", hdr)
	}
	if got["model"] != "claude-sonnet-5-5" || got["system"] != "be brief" || got["max_tokens"].(float64) != 4096 {
		t.Errorf("body = %v", got)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 || msgs[0].(map[string]any)["content"] != "a\n\nb" {
		t.Errorf("messages = %v", msgs)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		transient bool
	}{{429, true}, {500, true}, {503, true}, {529, true}, {400, false}, {401, false}, {404, false}}
	for _, tc := range cases {
		s := server(t, tc.status, `{"error":{"type":"x","message":"nope"}}`, nil, nil)
		c := &anthropic.Client{APIKey: "k", BaseURL: s.URL, DefaultModel: "m"}
		_, err := c.Complete(context.Background(), ports.LLMRequest{Messages: []ports.Message{{Role: "user", Content: "hi"}}})
		if err == nil {
			t.Fatalf("%d: no error", tc.status)
		}
		if errors.Is(err, ports.ErrTransient) != tc.transient {
			t.Errorf("%d: transient=%v, want %v (%v)", tc.status, errors.Is(err, ports.ErrTransient), tc.transient, err)
		}
	}
}

func TestInputValidation(t *testing.T) {
	c := &anthropic.Client{APIKey: "k", BaseURL: "http://127.0.0.1:1", DefaultModel: "m"}
	user := []ports.Message{{Role: "user", Content: "hi"}}
	if _, err := (&anthropic.Client{BaseURL: c.BaseURL, DefaultModel: "m"}).Complete(context.Background(), ports.LLMRequest{Messages: user}); err == nil {
		t.Error("missing key accepted")
	}
	if _, err := (&anthropic.Client{APIKey: "k", BaseURL: c.BaseURL}).Complete(context.Background(), ports.LLMRequest{Messages: user}); err == nil {
		t.Error("missing model accepted")
	}
	if _, err := c.Complete(context.Background(), ports.LLMRequest{Messages: []ports.Message{{Role: "assistant", Content: "x"}}}); err == nil {
		t.Error("conversation not starting with user accepted")
	}
	if _, err := c.Complete(context.Background(), ports.LLMRequest{Messages: []ports.Message{{Role: "tool", Content: "x"}}}); err == nil {
		t.Error("unknown role accepted")
	}
	// Unreachable server: a network error is transient.
	_, err := c.Complete(context.Background(), ports.LLMRequest{Messages: user})
	if !errors.Is(err, ports.ErrTransient) {
		t.Errorf("network failure should be transient: %v", err)
	}
}

func TestEmptyAndCancelled(t *testing.T) {
	s := server(t, 200, `{"content":[],"usage":{}}`, nil, nil)
	c := &anthropic.Client{APIKey: "k", BaseURL: s.URL, DefaultModel: "m"}
	if _, err := c.Complete(context.Background(), ports.LLMRequest{Messages: []ports.Message{{Role: "user", Content: "hi"}}}); err == nil || !strings.Contains(err.Error(), "no text") {
		t.Errorf("empty content: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Complete(ctx, ports.LLMRequest{Messages: []ports.Message{{Role: "user", Content: "hi"}}}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: %v", err)
	}
}
