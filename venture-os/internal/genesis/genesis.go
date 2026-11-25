// Package genesis runs the "Conversational Company Creation" interview.
//
// The language model proposes; deterministic code disposes. Each turn the model replies with a JSON envelope
// {"message", "blueprint", "ready"}. Any blueprint it proposes is decoded strictly (unknown fields are errors),
// then checked by domain.Blueprint.Validate. Problems are fed back to the model for repair a bounded number of
// times; the founder only ever sees a draft that passed validation, and "ready" is never trusted on the model's
// word alone.
package genesis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/ports"
)

const (
	// MaxFounderMessageBytes bounds a single founder message.
	MaxFounderMessageBytes = 8000
	// MaxHistoryMessages is how much conversation is replayed to the model each turn.
	MaxHistoryMessages = 40
	defaultRepairs     = 2
)

// ErrMessageTooLarge is returned when the founder's message exceeds MaxFounderMessageBytes.
var ErrMessageTooLarge = errors.New("genesis: founder message too large")

// Message is one turn of the founder / Venture Architect conversation.
type Message struct {
	Role    string // "founder" | "assistant"
	Content string
}

// Turn is the input to one engine step.
type Turn struct {
	History        []Message
	Draft          *domain.Blueprint // last validated draft, if any
	FounderMessage string
}

// Reply is the output of one engine step.
type Reply struct {
	// Message is what to show the founder.
	Message string
	// Draft is the latest blueprint that passed validation (the new one if the model produced a valid one this
	// turn, otherwise the previous draft). Nil until a first valid draft exists.
	Draft *domain.Blueprint
	// DraftChanged reports whether Draft was replaced this turn.
	DraftChanged bool
	// Ready means Draft is complete and valid and the founder can approve it.
	Ready bool
	// Problems lists validation problems that remained after all repair attempts, if any.
	Problems []string
	// Tokens consumed across every model call this turn.
	InputTokens, OutputTokens int
}

// Engine drives the interview.
type Engine struct {
	LLM   ports.LLM
	Model string
	// MaxRepairs is how many times validation problems are sent back to the model. Zero means the default (2).
	MaxRepairs int
}

type envelope struct {
	Message   string          `json:"message"`
	Blueprint json.RawMessage `json:"blueprint"`
	Ready     bool            `json:"ready"`
}

// Step advances the interview by one founder message.
func (e *Engine) Step(ctx context.Context, t Turn) (Reply, error) {
	msg := strings.TrimSpace(t.FounderMessage)
	if msg == "" {
		return Reply{}, errors.New("genesis: empty founder message")
	}
	if len(msg) > MaxFounderMessageBytes {
		return Reply{}, ErrMessageTooLarge
	}
	if e.LLM == nil {
		return Reply{}, errors.New("genesis: no LLM configured")
	}
	repairs := e.MaxRepairs
	if repairs <= 0 {
		repairs = defaultRepairs
	}

	msgs := []ports.Message{{Role: "system", Content: systemPrompt(t.Draft)}}
	hist := t.History
	if len(hist) > MaxHistoryMessages {
		hist = hist[len(hist)-MaxHistoryMessages:]
	}
	for _, m := range hist {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		msgs = append(msgs, ports.Message{Role: role, Content: m.Content})
	}
	msgs = append(msgs, ports.Message{Role: "user", Content: msg})

	reply := Reply{Draft: t.Draft}
	var lastMessage string
	for attempt := 0; ; attempt++ {
		resp, err := e.LLM.Complete(ctx, ports.LLMRequest{Model: e.Model, Messages: msgs, MaxTokens: 8000})
		if err != nil {
			return reply, fmt.Errorf("genesis: model call failed: %w", err)
		}
		reply.InputTokens += resp.InputTokens
		reply.OutputTokens += resp.OutputTokens

		env, problems := parseEnvelope(resp.Content)
		if env.Message != "" {
			lastMessage = env.Message
		}
		var bp *domain.Blueprint
		if len(problems) == 0 && hasBlueprint(env.Blueprint) {
			var bpProblems []string
			bp, bpProblems = decodeBlueprint(env.Blueprint)
			problems = append(problems, bpProblems...)
		}

		if len(problems) == 0 {
			reply.Message = env.Message
			if bp != nil {
				reply.Draft, reply.DraftChanged = bp, true
			}
			// "Ready" additionally requires a valid draft to exist: the model cannot declare victory without one.
			reply.Ready = env.Ready && reply.Draft != nil
			reply.Problems = nil
			if env.Ready && reply.Draft == nil {
				reply.Message = strings.TrimSpace(env.Message + "\n\nI still need to put the plan together before you can approve it.")
			}
			return reply, nil
		}

		if attempt >= repairs {
			reply.Message = lastMessage
			if reply.Message == "" {
				reply.Message = "I couldn't turn that into a valid plan yet. Could you tell me a little more?"
			}
			reply.Ready = false
			reply.Problems = problems
			return reply, nil
		}
		msgs = append(msgs,
			ports.Message{Role: "assistant", Content: resp.Content},
			ports.Message{Role: "user", Content: repairPrompt(problems)})
	}
}

func hasBlueprint(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}

// parseEnvelope tolerates a fenced code block around the JSON, which models add despite instructions.
func parseEnvelope(content string) (envelope, []string) {
	body := strings.TrimSpace(content)
	if strings.HasPrefix(body, "```") {
		body = strings.TrimPrefix(body, "```json")
		body = strings.TrimPrefix(body, "```")
		body = strings.TrimSuffix(strings.TrimSpace(body), "```")
	}
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(body)))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return envelope{}, []string{"your reply was not a single JSON object with keys message, blueprint, ready: " + err.Error()}
	}
	if dec.More() {
		return envelope{}, []string{"your reply contained more than one JSON value; reply with exactly one object"}
	}
	if strings.TrimSpace(env.Message) == "" {
		return env, []string{"\"message\" must be a non-empty string addressed to the founder"}
	}
	return env, nil
}

func decodeBlueprint(raw json.RawMessage) (*domain.Blueprint, []string) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var bp domain.Blueprint
	if err := dec.Decode(&bp); err != nil {
		return nil, []string{"blueprint JSON does not match the schema: " + err.Error()}
	}
	bp.SchemaVersion = domain.BlueprintSchemaVersion // the model is not asked to manage versioning
	if err := bp.Validate(); err != nil {
		var ve *domain.ValidationError
		if errors.As(err, &ve) {
			return nil, ve.Problems
		}
		return nil, []string{err.Error()}
	}
	return &bp, nil
}

func repairPrompt(problems []string) string {
	var b strings.Builder
	b.WriteString("SYSTEM VALIDATION FAILED. Fix every problem below and reply again with the same JSON envelope. ")
	b.WriteString("Do not mention this validation to the founder; just send the corrected result.\n")
	for _, p := range problems {
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}
