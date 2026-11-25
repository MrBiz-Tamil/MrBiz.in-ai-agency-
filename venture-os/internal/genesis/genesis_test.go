package genesis_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/genesis"
	"github.com/mrbiz/venture-os/internal/ports"
)

func envelope(t *testing.T, message string, bp any, ready bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"message": message, "blueprint": bp, "ready": ready})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func setup(t *testing.T, replies ...string) (*genesis.Engine, *fakes.LLM) {
	t.Helper()
	llm := fakes.NewLLM(fakes.NewHub())
	llm.Script(replies...)
	return &genesis.Engine{LLM: llm, Model: "test-model"}, llm
}

func TestQuestionTurnProducesNoDraft(t *testing.T) {
	e, _ := setup(t, envelope(t, "Which country will you operate from?", nil, false))
	r, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "I want to export spices"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Draft != nil || r.Ready || r.DraftChanged {
		t.Fatalf("unexpected draft/ready: %+v", r)
	}
	if r.Message != "Which country will you operate from?" {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestValidBlueprintBecomesDraftAndReady(t *testing.T) {
	bp := fixtures.EXIMBlueprint()
	bp.SchemaVersion = "" // the model is not asked to send it; the engine sets it
	e, llm := setup(t, envelope(t, "Here is your plan. Approve?", bp, true))
	r, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "Yes, looks right"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Draft == nil || !r.DraftChanged || !r.Ready {
		t.Fatalf("want ready draft, got %+v", r)
	}
	if r.Draft.SchemaVersion != domain.BlueprintSchemaVersion {
		t.Fatal("engine must stamp the schema version")
	}
	if n := len(llm.Requests()); n != 1 {
		t.Fatalf("model calls = %d, want 1", n)
	}
}

func TestInvalidBlueprintIsRepairedWithProblemsFedBack(t *testing.T) {
	bad := fixtures.EXIMBlueprint()
	bad.Pricing = nil
	bad.Business.Slug = "Bad Slug"
	good := fixtures.EXIMBlueprint()
	e, llm := setup(t,
		envelope(t, "Draft v1", bad, true),
		envelope(t, "Draft v2", good, true))
	r, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Ready || r.Draft == nil || r.Message != "Draft v2" {
		t.Fatalf("repair did not succeed: %+v", r)
	}
	reqs := llm.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(reqs))
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1].Content
	for _, want := range []string{"SYSTEM VALIDATION FAILED", "business.slug", "at least one rule"} {
		if !strings.Contains(last, want) {
			t.Errorf("repair prompt missing %q:\n%s", want, last)
		}
	}
}

func TestRepairIsBoundedAndFounderNeverSeesAnInvalidDraft(t *testing.T) {
	bad := fixtures.EXIMBlueprint()
	bad.Agents = nil
	prev := fixtures.EXIMBlueprint()
	e, llm := setup(t, envelope(t, "try 1", bad, true), envelope(t, "try 2", bad, true), envelope(t, "try 3", bad, true), envelope(t, "unused", bad, true))
	r, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "go", Draft: &prev})
	if err != nil {
		t.Fatal(err)
	}
	if r.Ready {
		t.Fatal("an invalid blueprint must never be ready")
	}
	if r.DraftChanged || r.Draft == nil || r.Draft.Business.Slug != prev.Business.Slug {
		t.Fatal("the previous valid draft must be kept")
	}
	if len(r.Problems) == 0 {
		t.Fatal("remaining problems must be reported")
	}
	if n := len(llm.Requests()); n != 3 { // 1 attempt + 2 repairs
		t.Fatalf("model calls = %d, want 3", n)
	}
}

func TestReadyWithoutDraftIsNotReady(t *testing.T) {
	e, _ := setup(t, envelope(t, "All set!", nil, true))
	r, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Ready {
		t.Fatal("model claimed ready with no draft")
	}
}

func TestUnknownFieldsAndNonJSONAreRepaired(t *testing.T) {
	good := envelope(t, "ok", fixtures.EXIMBlueprint(), false)
	withExtra := strings.Replace(envelope(t, "x", fixtures.EXIMBlueprint(), false), `"ready":false`, `"ready":false,"thoughts":"hmm"`, 1)
	e, llm := setup(t, "Sure! Here you go: not json", withExtra, good)
	r, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Draft == nil || r.Message != "ok" {
		t.Fatalf("not repaired: %+v", r)
	}
	if n := len(llm.Requests()); n != 3 {
		t.Fatalf("model calls = %d, want 3", n)
	}
}

func TestHallucinatedBlueprintFieldIsRejected(t *testing.T) {
	raw, _ := json.Marshal(fixtures.EXIMBlueprint())
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["secret_admin_mode"] = true
	e, llm := setup(t, envelope(t, "x", m, true), envelope(t, "fixed", fixtures.EXIMBlueprint(), true))
	r, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Message != "fixed" {
		t.Fatalf("unknown field not repaired: %+v", r)
	}
	if !strings.Contains(llm.Requests()[1].Messages[len(llm.Requests()[1].Messages)-1].Content, "secret_admin_mode") {
		t.Error("repair prompt should name the unknown field")
	}
}

func TestFencedJSONIsAccepted(t *testing.T) {
	e, _ := setup(t, "```json\n"+envelope(t, "hello", nil, false)+"\n```")
	r, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "hi"})
	if err != nil || r.Message != "hello" {
		t.Fatalf("fenced reply: %+v, %v", r, err)
	}
}

func TestPromptIncludesDraftHistoryAndRolesInOrder(t *testing.T) {
	draft := fixtures.EXIMBlueprint()
	e, llm := setup(t, envelope(t, "ok", nil, false))
	_, err := e.Step(context.Background(), genesis.Turn{
		Draft: &draft,
		History: []genesis.Message{
			{Role: "founder", Content: "I export spices"},
			{Role: "assistant", Content: "Where from?"},
		},
		FounderMessage: "Kerala",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := llm.Requests()[0].Messages
	if m[0].Role != "system" || !strings.Contains(m[0].Content, "CURRENT DRAFT") || !strings.Contains(m[0].Content, "kerala-spice-exports") {
		t.Fatal("system prompt must carry the current draft")
	}
	got := []string{}
	for _, x := range m[1:] {
		got = append(got, x.Role+":"+x.Content)
	}
	want := []string{"user:I export spices", "assistant:Where from?", "user:Kerala"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages = %v", got)
	}
}

func TestHistoryIsTrimmed(t *testing.T) {
	e, llm := setup(t, envelope(t, "ok", nil, false))
	var hist []genesis.Message
	for i := 0; i < 100; i++ {
		hist = append(hist, genesis.Message{Role: "founder", Content: "m"})
	}
	if _, err := e.Step(context.Background(), genesis.Turn{History: hist, FounderMessage: "x"}); err != nil {
		t.Fatal(err)
	}
	if n := len(llm.Requests()[0].Messages); n != 1+genesis.MaxHistoryMessages+1 {
		t.Fatalf("messages sent = %d", n)
	}
}

func TestInputGuards(t *testing.T) {
	e, _ := setup(t)
	if _, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "   "}); err == nil {
		t.Error("empty message accepted")
	}
	huge := strings.Repeat("x", genesis.MaxFounderMessageBytes+1)
	if _, err := e.Step(context.Background(), genesis.Turn{FounderMessage: huge}); !errors.Is(err, genesis.ErrMessageTooLarge) {
		t.Errorf("oversize message: %v", err)
	}
	if _, err := (&genesis.Engine{}).Step(context.Background(), genesis.Turn{FounderMessage: "x"}); err == nil {
		t.Error("missing LLM accepted")
	}
}

func TestModelErrorsPropagate(t *testing.T) {
	hub := fakes.NewHub()
	hub.Fail("llm", "Complete", ports.ErrTransient, 1)
	e := &genesis.Engine{LLM: fakes.NewLLM(hub)}
	if _, err := e.Step(context.Background(), genesis.Turn{FounderMessage: "hi"}); !errors.Is(err, ports.ErrTransient) {
		t.Fatalf("want transient error, got %v", err)
	}
}
