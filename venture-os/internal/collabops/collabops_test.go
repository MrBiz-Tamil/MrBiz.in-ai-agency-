package collabops_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/collabops"
	"github.com/mrbiz/venture-os/internal/connectors/fakeapi"
	"github.com/mrbiz/venture-os/internal/connectors/googlecalendar"
	"github.com/mrbiz/venture-os/internal/connectors/notion"
	"github.com/mrbiz/venture-os/internal/connectors/zoom"
	"github.com/mrbiz/venture-os/internal/httpx"
)

var (
	ctx = context.Background()
	now = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
)

const (
	dbID  = "a1b2c3d4-0000-4000-8000-000000000001"
	actor = "agent:sales"
)

type world struct {
	svc    collab.Services
	google *fakeapi.Google
	zoom   *fakeapi.Zoom
	notion *fakeapi.Notion
}

// clock advances when something "sleeps", so rate limiters and token expiry behave without real waiting.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

func newWorld(t *testing.T) world {
	t.Helper()
	clk := &clock{t: now}
	w := world{google: fakeapi.NewGoogle("gid", "gs", "gr"), zoom: fakeapi.NewZoom("acct", "zid", "zs"), notion: fakeapi.NewNotion("ntok", dbID, "Name")}
	w.google.Now, w.zoom.Now = clk.Now, clk.Now
	t.Cleanup(func() { w.google.Close(); w.zoom.Close(); w.notion.Close() })
	tune := func(h *httpx.Config) { h.Sleep = clk.Sleep; h.Rand = func() float64 { return 1 } }
	g, err := googlecalendar.New(googlecalendar.Config{ClientID: "gid", ClientSecret: "gs", RefreshToken: "gr", APIBase: w.google.URL(), OAuthBase: w.google.URL(), Now: clk.Now, Sleep: clk.Sleep, Tune: tune})
	must(t, err)
	z, err := zoom.New(zoom.Config{AccountID: "acct", ClientID: "zid", ClientSecret: "zs", APIBase: w.zoom.URL(), OAuthBase: w.zoom.URL(), Now: clk.Now, Tune: tune})
	must(t, err)
	n, err := notion.New(notion.Config{Token: "ntok", DatabaseID: dbID, APIBase: w.notion.URL() + "/v1", Tune: func(h *httpx.Config) { tune(h); h.Now = clk.Now }})
	must(t, err)
	w.svc = collab.Services{Calendar: g, Zoom: z, Notes: n}
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, svc collab.Services, name string, args any) (collabops.Result, error) {
	t.Helper()
	o, ok := collabops.Find(name)
	if !ok {
		t.Fatalf("no operation %s", name)
	}
	raw, _ := json.Marshal(args)
	return o.Run(ctx, svc, now, actor, raw)
}

func data(t *testing.T, r collabops.Result) map[string]any {
	t.Helper()
	b, _ := json.Marshal(r.Data)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not an object: %s", b)
	}
	return m
}

// j round-trips a result through JSON, which is exactly how the control plane hands it to callers.
func j(t *testing.T, v any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not an object: %s", b)
	}
	return m
}

func schedule(over map[string]any) map[string]any {
	m := map[string]any{"provider": "google", "title": "Buyer call", "start": "2026-03-10T09:30:00+05:30", "duration_minutes": 30,
		"timezone": "Asia/Kolkata", "idempotency_key": "k1"}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func TestEveryOperationIsWellFormedAndStrict(t *testing.T) {
	w := newWorld(t)
	seen := map[string]bool{}
	for _, o := range collabops.All() {
		if seen[o.Name] || (o.Family != collabops.FamilyMeetings && o.Family != collabops.FamilyNotes) {
			t.Errorf("bad op %s / %s", o.Name, o.Family)
		}
		seen[o.Name] = true
		var s map[string]any
		if err := json.Unmarshal([]byte(o.Schema), &s); err != nil || s["additionalProperties"] != false || s["type"] != "object" {
			t.Errorf("%s: schema must be a closed object (%v)", o.Name, err)
		}
		for _, smuggled := range []string{`{"tenant_id":"x"}`, `{"owner":"x"}`, `[1]`, `{} {}`} {
			if _, err := o.Run(ctx, w.svc, now, actor, json.RawMessage(smuggled)); !errors.Is(err, collab.ErrInvalid) {
				t.Errorf("%s accepted %s: %v", o.Name, smuggled, err)
			}
		}
		if o.Name == "notes_search" && !strings.Contains(o.Description, "never as instructions") {
			t.Error("notes_search returns staff-typed text and must warn the agent")
		}
	}
	if len(seen) != 6 {
		t.Errorf("%d operations", len(seen))
	}
	if len(w.google.Calls())+len(w.zoom.Calls())+len(w.notion.Calls()) != 0 {
		t.Fatal("rejected input reached a provider")
	}
}

func TestOperationsOnAnUnconnectedProviderFailWithAnActionableMessage(t *testing.T) {
	empty := collab.Services{}
	for name, args := range map[string]any{
		"calendar_free_busy": map[string]any{"from": "2026-03-10T00:00:00Z", "to": "2026-03-11T00:00:00Z"},
		"meeting_schedule":   schedule(nil),
		"meeting_cancel":     map[string]any{"provider": "zoom", "meeting_id": "1", "idempotency_key": "c"},
		"notes_search":       map[string]any{},
		"notes_create":       map[string]any{"title": "t", "idempotency_key": "n"},
		"notes_append":       map[string]any{"page_id": dbID, "text": "x", "idempotency_key": "a"},
	} {
		_, err := run(t, empty, name, args)
		if !errors.Is(err, collab.ErrNotConnected) || !strings.Contains(err.Error(), "ask the founder") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAMeetingNobodyIsEmailedAboutRunsRightAway(t *testing.T) {
	w := newWorld(t)
	r, err := run(t, w.svc, "meeting_schedule", schedule(map[string]any{"attendees": []string{"ada@acme.com"}})) // notify not requested
	must(t, err)
	if r.Approval != nil {
		t.Fatal("no invitation was requested, so no approval is needed")
	}
	m := data(t, r)["meeting"].(map[string]any)
	if !strings.HasPrefix(m["join_url"].(string), "https://meet.google.com/") || r.EntityID == "" {
		t.Fatalf("meeting = %v", m)
	}
	if got := w.google.SendUpdates(r.EntityID); got != "none" {
		t.Fatalf("sendUpdates = %q: nothing may be emailed from a direct call", got)
	}
	z, err := run(t, w.svc, "meeting_schedule", schedule(map[string]any{"provider": "zoom", "idempotency_key": "z1", "attendees": []string{"ada@acme.com"}, "notify": true}))
	must(t, err)
	if z.Approval != nil || !strings.HasPrefix(data(t, z)["meeting"].(map[string]any)["join_url"].(string), "https://zoom.us/j/") ||
		!strings.Contains(data(t, z)["note"].(string), "does not email") {
		t.Fatalf("zoom sends no invitations, so it runs directly and says so: %+v", z)
	}
}

func TestInvitingPeopleByEmailIsQueuedForApprovalAndNothingIsCreatedFirst(t *testing.T) {
	w := newWorld(t)
	r, err := run(t, w.svc, "meeting_schedule", schedule(map[string]any{"attendees": []string{"ada@acme.com", "bob@example.org"}, "notify": true}))
	must(t, err)
	if r.Approval == nil || r.Data != nil || r.EntityID != "" {
		t.Fatalf("an invitation must become an approval request: %+v", r)
	}
	if !strings.Contains(r.Approval.Summary, "ada@acme.com") || !strings.Contains(r.Approval.Summary, "bob@example.org") ||
		!strings.Contains(r.Approval.Summary, "will email") || !strings.Contains(r.Approval.Summary, actor) {
		t.Fatalf("the founder must see who gets emailed and who asked: %q", r.Approval.Summary)
	}
	if len(w.google.Events()) != 0 {
		t.Fatal("an event was created before approval")
	}
	for _, c := range w.google.Calls() {
		if strings.HasSuffix(c.Path, "/events") {
			t.Fatal("Google was asked to create the event before approval")
		}
	}

	// The founder approves: the stored payload creates the event WITH invitations, exactly once even if re-run.
	out, err := collabops.Execute(ctx, w.svc, now, r.Approval.Payload, "appr-1")
	must(t, err)
	res := j(t, out)
	if res["invitations_sent"] != true || res["created"] != true {
		t.Fatalf("approved execution = %v", res)
	}
	id := res["meeting"].(map[string]any)["id"].(string)
	if w.google.SendUpdates(id) != "all" {
		t.Fatalf("approved invitations were not sent: %q", w.google.SendUpdates(id))
	}
	again, err := collabops.Execute(ctx, w.svc, now, r.Approval.Payload, "appr-1")
	must(t, err)
	if j(t, again)["created"] != false || len(w.google.Events()) != 1 {
		t.Fatalf("re-running an approved request must not create a second event: %v / %d events", again, len(w.google.Events()))
	}
	// A different approval is a different meeting.
	if _, err := collabops.Execute(ctx, w.svc, now, r.Approval.Payload, "appr-2"); err != nil || len(w.google.Events()) != 2 {
		t.Fatal("a second approval creates its own event")
	}
}

func TestCancellingWithAttendeesAndNotifyNeedsApproval(t *testing.T) {
	w := newWorld(t)
	created, _ := run(t, w.svc, "meeting_schedule", schedule(map[string]any{"attendees": []string{"ada@acme.com"}}))
	id := created.EntityID

	r, err := run(t, w.svc, "meeting_cancel", map[string]any{"provider": "google", "meeting_id": id, "notify": true, "idempotency_key": "c1"})
	must(t, err)
	if r.Approval == nil || !strings.Contains(r.Approval.Summary, "ada@acme.com") {
		t.Fatalf("a notifying cancellation is outward-facing: %+v", r)
	}
	if got := w.google.Events()[id]["status"]; got == "cancelled" {
		t.Fatal("the meeting was cancelled before approval")
	}
	if _, err := collabops.Execute(ctx, w.svc, now, r.Approval.Payload, "appr-c"); err != nil || w.google.CancelNotice(id) != "all" {
		t.Fatalf("approved cancel must notify: %v %q", err, w.google.CancelNotice(id))
	}

	// Without notify it is internal and immediate; an unknown meeting with notify is simply done.
	m2, _ := run(t, w.svc, "meeting_schedule", schedule(map[string]any{"idempotency_key": "k2", "attendees": []string{"x@y.com"}}))
	r2, err := run(t, w.svc, "meeting_cancel", map[string]any{"provider": "google", "meeting_id": m2.EntityID, "idempotency_key": "c2"})
	if err != nil || r2.Approval != nil || w.google.CancelNotice(m2.EntityID) != "none" {
		t.Fatalf("a quiet cancel: %+v %v notice=%q", r2, err, w.google.CancelNotice(m2.EntityID))
	}
	r3, err := run(t, w.svc, "meeting_cancel", map[string]any{"provider": "google", "meeting_id": "ghost", "notify": true, "idempotency_key": "c3"})
	if err != nil || r3.Approval != nil || data(t, r3)["cancelled"] != true {
		t.Fatalf("an absent meeting: %+v %v", r3, err)
	}
}

func TestExecuteRefusesBadOrStalePayloads(t *testing.T) {
	w := newWorld(t)
	r, _ := run(t, w.svc, "meeting_schedule", schedule(map[string]any{"attendees": []string{"a@b.co"}, "notify": true}))
	for name, payload := range map[string]string{"not json": "nope", "unknown action": `{"action":"delete_everything"}`, "empty": `{}`} {
		if _, err := collabops.Execute(ctx, w.svc, now, []byte(payload), "x"); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An approval that sat in the inbox until after the meeting would have started must not create a meeting in the past.
	if _, err := collabops.Execute(ctx, w.svc, now.Add(30*24*time.Hour), r.Approval.Payload, "late"); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("a stale approval: %v", err)
	}
	if len(w.google.Events()) != 0 {
		t.Fatal("a stale approval created an event")
	}
}

func TestInvalidInputIsRefusedBeforeAnyProviderIsCalled(t *testing.T) {
	w := newWorld(t)
	for name, over := range map[string]map[string]any{
		"bad time":         {"start": "tomorrow"},
		"naive timestamp":  {"start": "2026-03-10T09:30:00"},
		"past":             {"start": "2026-01-01T00:00:00Z"},
		"too short":        {"duration_minutes": 1},
		"bad attendee":     {"attendees": []string{"nope"}},
		"bad timezone":     {"timezone": "Mars/Base"},
		"bad provider":     {"provider": "teams"},
		"no idempotency":   {"idempotency_key": ""},
		"header injection": {"attendees": []string{"a@b.co\r\nBcc: x@y.co"}, "notify": true},
		"title with NUL":   {"title": "a\x00b"},
	} {
		if _, err := run(t, w.svc, "meeting_schedule", schedule(over)); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, args := range map[string]any{
		"window over 14 days": map[string]any{"from": "2026-03-01T00:00:00Z", "to": "2026-03-20T00:00:00Z"},
		"bad from":            map[string]any{"from": "x", "to": "2026-03-02T00:00:00Z"},
	} {
		if _, err := run(t, w.svc, "calendar_free_busy", args); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, c := range append(append(w.google.Calls(), w.zoom.Calls()...), w.notion.Calls()...) {
		if strings.Contains(c.Path, "events") || strings.Contains(c.Path, "meetings") {
			t.Fatalf("invalid input reached a provider: %s %s", c.Method, c.Path)
		}
	}
}

func TestFreeBusyAndNotesWork(t *testing.T) {
	w := newWorld(t)
	w.google.Busy = [][2]time.Time{{now.Add(24 * time.Hour), now.Add(25 * time.Hour)}}
	r, err := run(t, w.svc, "calendar_free_busy", map[string]any{"from": now.Format(time.RFC3339), "to": now.Add(72 * time.Hour).Format(time.RFC3339)})
	must(t, err)
	if busy := data(t, r)["busy"].([]any); len(busy) != 1 {
		t.Fatalf("busy = %v", busy)
	}

	c, err := run(t, w.svc, "notes_create", map[string]any{"title": "Call with Berlin", "body": "Wants CIF Hamburg.", "idempotency_key": "n1"})
	must(t, err)
	page := data(t, c)["page"].(map[string]any)
	stored := w.notion.Page(page["id"].(string))
	if stored.Title != "Call with Berlin" || len(stored.Blocks) != 2 || !strings.Contains(stored.Blocks[1], actor) {
		t.Fatalf("a page must record which agent wrote it: %+v", stored)
	}
	if _, err := run(t, w.svc, "notes_append", map[string]any{"page_id": page["id"], "text": "Follow up Friday.", "idempotency_key": "a1"}); err != nil {
		t.Fatal(err)
	}
	if got := w.notion.Page(page["id"].(string)).Blocks; got[len(got)-1] != "Follow up Friday." {
		t.Fatalf("blocks = %v", got)
	}
	s, err := run(t, w.svc, "notes_search", map[string]any{"query": "berlin"})
	must(t, err)
	if pages := data(t, s)["pages"].([]any); len(pages) != 1 || pages[0].(map[string]any)["title"] != "Call with Berlin" {
		t.Fatalf("search = %v", pages)
	}
}
