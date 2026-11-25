// Package anthropic implements ports.LLM on the Anthropic Messages API using only net/http.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mrbiz/venture-os/internal/ports"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	maxBody        = 8 << 20
)

// Client calls the Messages API.
type Client struct {
	APIKey  string
	BaseURL string       // default https://api.anthropic.com
	HTTP    *http.Client // default: 120s timeout
	// DefaultModel is used when a request has no model.
	DefaultModel string
}

var _ ports.LLM = (*Client)(nil)

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    string       `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
}

type apiResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements ports.LLM. 429, 5xx and 529 (overloaded) are reported as ports.ErrTransient.
func (c *Client) Complete(ctx context.Context, req ports.LLMRequest) (ports.LLMResponse, error) {
	if c.APIKey == "" {
		return ports.LLMResponse{}, errors.New("anthropic: API key not configured")
	}
	model := req.Model
	if model == "" {
		model = c.DefaultModel
	}
	if model == "" {
		return ports.LLMResponse{}, errors.New("anthropic: no model specified")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	var system []string
	var msgs []apiMessage
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			system = append(system, m.Content)
		case "user", "assistant":
			// The API requires strictly alternating roles: merge consecutive turns of the same role.
			if n := len(msgs); n > 0 && msgs[n-1].Role == m.Role {
				msgs[n-1].Content += "\n\n" + m.Content
			} else {
				msgs = append(msgs, apiMessage{Role: m.Role, Content: m.Content})
			}
		default:
			return ports.LLMResponse{}, fmt.Errorf("anthropic: unsupported message role %q", m.Role)
		}
	}
	if len(msgs) == 0 || msgs[0].Role != "user" {
		return ports.LLMResponse{}, errors.New("anthropic: conversation must start with a user message")
	}

	body, err := json.Marshal(apiRequest{Model: model, MaxTokens: maxTokens, System: strings.Join(system, "\n\n"), Messages: msgs})
	if err != nil {
		return ports.LLMResponse{}, err
	}
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return ports.LLMResponse{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return ports.LLMResponse{}, ctx.Err()
		}
		return ports.LLMResponse{}, fmt.Errorf("anthropic: request failed: %w: %w", err, ports.ErrTransient)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return ports.LLMResponse{}, fmt.Errorf("anthropic: reading response: %w: %w", err, ports.ErrTransient)
	}

	var out apiResponse
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("anthropic: HTTP %d", resp.StatusCode)
		if out.Error != nil {
			msg += ": " + out.Error.Type + ": " + out.Error.Message
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return ports.LLMResponse{}, fmt.Errorf("%s: %w", msg, ports.ErrTransient)
		}
		return ports.LLMResponse{}, errors.New(msg)
	}

	var text strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	if text.Len() == 0 {
		return ports.LLMResponse{}, errors.New("anthropic: response contained no text")
	}
	return ports.LLMResponse{Content: text.String(), InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}, nil
}
