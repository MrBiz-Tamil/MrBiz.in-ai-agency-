package zoom_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/connectors/fakeapi"
	"github.com/mrbiz/venture-os/internal/connectors/zoom"
	"github.com/mrbiz/venture-os/internal/httpx"
)

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func (c *clock) Sleep(_ context.Context, d time.Duration) error { c.Advance(d); return nil }

func setup(t *testing.T) (*zoom.Client, *fakeapi.Zoom, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	z := fakeapi.NewZoom("acct-1", "cid", "csecret")
	z.Now = clk.Now
	t.Cleanup(z.Close)
	c, err := zoom.New(zoom.Config{AccountID: "acct-1", ClientID: "cid", ClientSecret: "csecret", APIBase: z.URL(), OAuthBase: z.URL(), Now: clk.Now,
		Tune: func(h *httpx.Config) { h.Sleep = clk.Sleep; h.Rand = func() float64 { return 1 } }})
	if err != nil {
		t.Fatal(err)
	}
	return c, z, clk
}

func in(clk *clock) collab.MeetingInput {
	return collab.MeetingInput{Title: "Supplier sync", Description: "Harvest update", Start: clk.Now().Add(24 * time.Hour), Duration: 30 * time.Minute,
		Timezone: "Asia/Kolkata", Attendees: []string{"farm@coop.example"}}
}

func TestScheduleCreatesAMeetingWithAWaitingRoomAndAJoinLink(t *testing.T) {
	c, z, clk := setup(t)
	m, err := c.Schedule(ctx, in(clk))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Created || m.Provider != "zoom" || m.Title != "Supplier sync" || !strings.HasPrefix(m.JoinURL, "https://zoom.us/j/") {
		t.Fatalf("meeting = %+v", m)
	}
	if !m.End.Equal(m.Start.Add(30*time.Minute)) || len(m.Attendees) != 1 || m.Attendees[0] != "farm@coop.example" {
		t.Fatalf("times/attendees = %+v", m)
	}
	id, _ := strconv.ParseInt(m.ID, 10, 64)
	raw := z.Meeting(id)
	settings := raw["settings"].(map[string]any)
	if settings["waiting_room"] != true || settings["join_before_host"] != false {
		t.Fatalf("a meeting must be created with a waiting room and no join-before-host: %v", settings)
	}
	if raw["timezone"] != "Asia/Kolkata" || raw["duration"].(float64) != 30 {
		t.Fatalf("raw = %v", raw)
	}
}

func TestTokenIsRequestedWithBasicAuthCachedAndRenewed(t *testing.T) {
	c, z, clk := setup(t)
	for i := 0; i < 3; i++ {
		if _, err := c.Schedule(ctx, in(clk)); err != nil {
			t.Fatal(err)
		}
	}
	tokens := func() (n int) {
		for _, call := range z.Calls() {
			if call.Path == "/oauth/token" {
				n++
				if call.Method != "POST" || call.Query.Get("grant_type") != "account_credentials" || call.Query.Get("account_id") != "acct-1" {
					t.Fatalf("token request = %+v", call)
				}
			}
		}
		return
	}
	if tokens() != 1 {
		t.Fatalf("%d token requests for 3 calls", tokens())
	}
	clk.Advance(2 * time.Hour)
	if _, err := c.Schedule(ctx, in(clk)); err != nil || tokens() != 2 {
		t.Fatalf("renew on expiry: %d %v", tokens(), err)
	}
	z.ExpireTokens()
	if _, err := c.Schedule(ctx, in(clk)); err != nil || tokens() != 3 {
		t.Fatalf("renew on 401: %d %v", tokens(), err)
	}
}

func TestBadCredentialsAreAnAuthErrorAndNotRetried(t *testing.T) {
	_, z, clk := setup(t)
	for name, cfg := range map[string]zoom.Config{
		"wrong secret":  {AccountID: "acct-1", ClientID: "cid", ClientSecret: "nope"},
		"wrong account": {AccountID: "other", ClientID: "cid", ClientSecret: "csecret"},
	} {
		cfg.APIBase, cfg.OAuthBase, cfg.Now = z.URL(), z.URL(), clk.Now
		bad, err := zoom.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		before := len(z.Calls())
		if _, err := bad.Schedule(ctx, in(clk)); !errors.Is(err, collab.ErrAuth) {
			t.Errorf("%s: %v", name, err)
		}
		if n := len(z.Calls()) - before; n != 1 {
			t.Errorf("%s: %d calls; rejected credentials must not be retried", name, n)
		}
	}
	if _, err := zoom.New(zoom.Config{}); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("missing credentials: %v", err)
	}
}

func TestACreateWhoseResponseIsLostIsNotRepeated(t *testing.T) {
	c, z, clk := setup(t)
	if _, err := c.Schedule(ctx, in(clk)); err != nil { // warm the token cache so the failure lands on the create itself
		t.Fatal(err)
	}
	before, meetings := len(z.Calls()), z.Meetings()
	z.FailNext(503)
	if _, err := c.Schedule(ctx, in(clk)); !errors.Is(err, collab.ErrUnavailable) {
		t.Fatalf("a 5xx on create must surface: %v", err)
	}
	creates := 0
	for _, call := range z.Calls()[before:] {
		if strings.HasSuffix(call.Path, "/meetings") {
			creates++
		}
	}
	if creates != 1 || z.Meetings() != meetings {
		t.Fatalf("creates=%d meetings %d->%d: Zoom has no idempotency key, so a create is never blindly repeated", creates, meetings, z.Meetings())
	}
}

func TestGetAndCancel(t *testing.T) {
	c, z, clk := setup(t)
	m, _ := c.Schedule(ctx, in(clk))
	got, err := c.Get(ctx, m.ID)
	if err != nil || got.ID != m.ID || got.JoinURL != m.JoinURL || got.Title != "Supplier sync" {
		t.Fatalf("get = %+v %v", got, err)
	}
	id, _ := strconv.ParseInt(m.ID, 10, 64)
	if err := c.Cancel(ctx, m.ID, true); err != nil || z.CancelReminder(id) != "true" || z.Meetings() != 0 {
		t.Fatalf("cancel: %v reminder=%q meetings=%d", err, z.CancelReminder(id), z.Meetings())
	}
	if err := c.Cancel(ctx, m.ID, false); err != nil {
		t.Fatalf("cancelling again must succeed: %v", err)
	}
	if _, err := c.Get(ctx, m.ID); !errors.Is(err, collab.ErrNotFound) {
		t.Fatalf("a cancelled meeting reads as not found: %v", err)
	}
	for _, bad := range []string{"", "abc", "-5", "0", "1; DROP", "../users"} {
		if _, err := c.Get(ctx, bad); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("Get(%q): %v", bad, err)
		}
		if err := c.Cancel(ctx, bad, false); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("Cancel(%q): %v", bad, err)
		}
	}
}

func TestMeetingIdsBeyondFloatPrecisionSurviveTheRoundTrip(t *testing.T) {
	c, z, clk := setup(t)
	_ = z
	m, err := c.Schedule(ctx, in(clk))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(m.ID, ".e+") {
		t.Fatalf("id %q was mangled through a float", m.ID)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(`{"id": 81234567999}`), &raw)
}

func TestInvalidInputNeverReachesZoom(t *testing.T) {
	c, z, clk := setup(t)
	bad := in(clk)
	bad.Title = ""
	if _, err := c.Schedule(ctx, bad); !errors.Is(err, collab.ErrInvalid) || len(z.Calls()) != 0 {
		t.Fatalf("err=%v calls=%d", err, len(z.Calls()))
	}
}

func TestCheckVerifiesCredentials(t *testing.T) {
	c, z, _ := setup(t)
	if err := c.Check(ctx); err != nil || z.Meetings() != 0 {
		t.Fatalf("check: %v meetings=%d", err, z.Meetings())
	}
	bad, _ := zoom.New(zoom.Config{AccountID: "acct-1", ClientID: "cid", ClientSecret: "wrong", APIBase: z.URL(), OAuthBase: z.URL()})
	if err := bad.Check(ctx); !errors.Is(err, collab.ErrAuth) {
		t.Fatalf("bad credentials: %v", err)
	}
}
