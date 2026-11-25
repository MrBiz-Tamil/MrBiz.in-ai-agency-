package googlecalendar_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/connectors/fakeapi"
	"github.com/mrbiz/venture-os/internal/connectors/googlecalendar"
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

func setup(t *testing.T) (*googlecalendar.Client, *fakeapi.Google, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	g := fakeapi.NewGoogle("client-id", "client-secret", "refresh-1")
	g.Now = clk.Now
	t.Cleanup(g.Close)
	c, err := googlecalendar.New(googlecalendar.Config{
		ClientID: "client-id", ClientSecret: "client-secret", RefreshToken: "refresh-1",
		APIBase: g.URL(), OAuthBase: g.URL(), Now: clk.Now, Sleep: clk.Sleep,
		Tune: func(h *httpx.Config) { h.Sleep = clk.Sleep; h.Rand = func() float64 { return 1 } },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, g, clk
}

func meeting(clk *clock) collab.MeetingInput {
	return collab.MeetingInput{Title: "Buyer call", Description: "Pepper pricing", Start: clk.Now().Add(48 * time.Hour), Duration: 45 * time.Minute,
		Timezone: "Asia/Kolkata", Attendees: []string{"ada@acme.com"}, Notify: true, RequestID: "req-1"}
}

func TestScheduleCreatesAnEventWithAMeetLinkAndInvites(t *testing.T) {
	c, g, clk := setup(t)
	m, err := c.Schedule(ctx, meeting(clk))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Created || m.Provider != "google" || m.Title != "Buyer call" || !strings.HasPrefix(m.JoinURL, "https://meet.google.com/") {
		t.Fatalf("meeting = %+v", m)
	}
	if !m.End.Equal(m.Start.Add(45*time.Minute)) || len(m.Attendees) != 1 || m.Attendees[0] != "ada@acme.com" {
		t.Fatalf("times/attendees = %+v", m)
	}
	if m.ID != googlecalendar.EventID("req-1") || !regexp.MustCompile(`^[a-v0-9]{26}$`).MatchString(m.ID) {
		t.Fatalf("event id %q is not derived from the request id in Google's alphabet", m.ID)
	}
	if g.SendUpdates(m.ID) != "all" {
		t.Fatalf("invitations were not requested: %q", g.SendUpdates(m.ID))
	}
	var sent map[string]any
	for _, call := range g.Calls() {
		if call.Method == "POST" && strings.HasSuffix(call.Path, "/events") {
			_ = json.Unmarshal(call.Body, &sent)
			if call.Query.Get("conferenceDataVersion") != "1" {
				t.Fatal("conferenceDataVersion=1 is required for Google to create a Meet link")
			}
			if call.Header.Get("Authorization") != "Bearer ya29.fake-1" {
				t.Fatalf("auth header = %q", call.Header.Get("Authorization"))
			}
		}
	}
	if sent["start"].(map[string]any)["timeZone"] != "Asia/Kolkata" {
		t.Fatalf("time zone lost: %v", sent["start"])
	}
}

func TestWithoutNotifyNoEmailIsSent(t *testing.T) {
	c, g, clk := setup(t)
	in := meeting(clk)
	in.Notify = false
	m, err := c.Schedule(ctx, in)
	if err != nil || g.SendUpdates(m.ID) != "none" {
		t.Fatalf("sendUpdates=%q err=%v: attendees must not be emailed unless asked", g.SendUpdates(m.ID), err)
	}
	in = meeting(clk)
	in.RequestID, in.Attendees = "req-solo", nil // notifying nobody is "none", not "all"
	m, _ = c.Schedule(ctx, in)
	if g.SendUpdates(m.ID) != "none" {
		t.Fatal("no attendees, nothing to send")
	}
}

func TestARetriedRequestReturnsTheExistingEventInsteadOfADuplicate(t *testing.T) {
	c, g, clk := setup(t)
	first, err := c.Schedule(ctx, meeting(clk))
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.Schedule(ctx, meeting(clk))
	if err != nil || again.Created || again.ID != first.ID || again.JoinURL != first.JoinURL {
		t.Fatalf("retry = %+v err=%v", again, err)
	}
	if n := len(g.Events()); n != 1 {
		t.Fatalf("%d events exist; a retry must not create another", n)
	}
	other := meeting(clk)
	other.RequestID = "req-2"
	if m, _ := c.Schedule(ctx, other); m.ID == first.ID || len(g.Events()) != 2 {
		t.Fatal("a different request id is a different meeting")
	}
}

func TestWithoutARequestIDEveryCallMakesANewEventAndIsNotRetriedBlindly(t *testing.T) {
	c, g, clk := setup(t)
	in := meeting(clk)
	in.RequestID = ""
	a, _ := c.Schedule(ctx, in)
	b, _ := c.Schedule(ctx, in)
	if a.ID == b.ID || len(g.Events()) != 2 {
		t.Fatal("expected two events")
	}
	before := len(g.Calls())
	g.FailNext(503)
	if _, err := c.Schedule(ctx, in); !errors.Is(err, collab.ErrUnavailable) {
		t.Fatalf("a 5xx on a create with no idempotency key must surface, not be repeated: %v", err)
	}
	creates := 0
	for _, call := range g.Calls()[before:] {
		if call.Method == "POST" && strings.HasSuffix(call.Path, "/events") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("the create was attempted %d times", creates)
	}
}

func TestAPendingMeetLinkIsPolledUntilItAppears(t *testing.T) {
	c, g, clk := setup(t)
	g.PendingMeet = true
	m, err := c.Schedule(ctx, meeting(clk))
	if err != nil || !strings.HasPrefix(m.JoinURL, "https://meet.google.com/") {
		t.Fatalf("meeting = %+v err=%v", m, err)
	}
}

func TestAccessTokenIsCachedRefreshedOnExpiryAndOn401(t *testing.T) {
	c, g, clk := setup(t)
	for i := 0; i < 3; i++ {
		in := meeting(clk)
		in.RequestID = "r" + string(rune('a'+i))
		if _, err := c.Schedule(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	countToken := func() (n int) {
		for _, call := range g.Calls() {
			if call.Path == "/token" {
				n++
			}
		}
		return
	}
	if countToken() != 1 {
		t.Fatalf("%d token requests for 3 calls: the access token must be cached", countToken())
	}
	clk.Advance(2 * time.Hour) // the token expired
	in := meeting(clk)
	in.RequestID = "after-expiry"
	if _, err := c.Schedule(ctx, in); err != nil || countToken() != 2 {
		t.Fatalf("refresh on expiry: tokens=%d err=%v", countToken(), err)
	}
	g.ExpireTokens() // Google invalidated it early: the 401 must trigger one silent refresh and a retry
	in.RequestID = "after-401"
	if _, err := c.Schedule(ctx, in); err != nil || countToken() != 3 {
		t.Fatalf("refresh on 401: tokens=%d err=%v", countToken(), err)
	}
}

func TestARevokedOrWrongCredentialIsAnAuthErrorNotARetryLoop(t *testing.T) {
	c, g, clk := setup(t)
	g.Revoke()
	_, err := c.Schedule(ctx, meeting(clk))
	if !errors.Is(err, collab.ErrAuth) || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("revoked token: %v", err)
	}
	if n := len(g.Calls()); n != 1 {
		t.Fatalf("%d calls: an invalid_grant must not be retried", n)
	}
	bad, _ := googlecalendar.New(googlecalendar.Config{ClientID: "client-id", ClientSecret: "WRONG", RefreshToken: "refresh-1", APIBase: g.URL(), OAuthBase: g.URL(), Now: clk.Now})
	if _, err := bad.Schedule(ctx, meeting(clk)); !errors.Is(err, collab.ErrAuth) {
		t.Fatalf("wrong client secret: %v", err)
	}
}

func TestGetCancelAndFreeBusy(t *testing.T) {
	c, g, clk := setup(t)
	m, _ := c.Schedule(ctx, meeting(clk))
	got, err := c.Get(ctx, m.ID)
	if err != nil || got.Title != "Buyer call" || got.JoinURL != m.JoinURL {
		t.Fatalf("get = %+v %v", got, err)
	}
	if _, err := c.Get(ctx, "nope"); !errors.Is(err, collab.ErrNotFound) {
		t.Fatalf("unknown event: %v", err)
	}
	if err := c.Cancel(ctx, m.ID, true); err != nil || g.CancelNotice(m.ID) != "all" {
		t.Fatalf("cancel: %v notice=%q", err, g.CancelNotice(m.ID))
	}
	if err := c.Cancel(ctx, m.ID, true); err != nil {
		t.Fatalf("cancelling twice must succeed (410 Gone): %v", err)
	}
	if err := c.Cancel(ctx, "never-existed", false); err != nil {
		t.Fatalf("cancelling an absent event must succeed: %v", err)
	}
	if _, err := c.Get(ctx, m.ID); !errors.Is(err, collab.ErrNotFound) {
		t.Fatalf("a cancelled event reads as not found: %v", err)
	}

	g.Busy = [][2]time.Time{{clk.Now().Add(24 * time.Hour), clk.Now().Add(25 * time.Hour)}}
	iv, err := c.FreeBusy(ctx, clk.Now(), clk.Now().Add(72*time.Hour))
	if err != nil || len(iv) != 1 || iv[0].End.Sub(iv[0].Start) != time.Hour {
		t.Fatalf("freebusy = %+v %v", iv, err)
	}
	if _, err := c.FreeBusy(ctx, clk.Now(), clk.Now().Add(-time.Hour)); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("a backwards window: %v", err)
	}
	if _, err := c.FreeBusy(ctx, clk.Now(), clk.Now().Add(100*24*time.Hour)); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("a window over 62 days: %v", err)
	}
}

func TestInvalidInputNeverReachesGoogle(t *testing.T) {
	c, g, clk := setup(t)
	for name, mut := range map[string]func(*collab.MeetingInput){
		"no title": func(m *collab.MeetingInput) { m.Title = "" },
		"past":     func(m *collab.MeetingInput) { m.Start = clk.Now().Add(-24 * time.Hour) },
		"attendee": func(m *collab.MeetingInput) { m.Attendees = []string{"bad"} },
	} {
		in := meeting(clk)
		mut(&in)
		if _, err := c.Schedule(ctx, in); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(g.Calls()) != 0 {
		t.Fatalf("%d calls were made for invalid input", len(g.Calls()))
	}
	if _, err := googlecalendar.New(googlecalendar.Config{}); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("missing credentials: %v", err)
	}
}

func TestQuotaAndOutagesAreUnavailableNotAuth(t *testing.T) {
	c, g, clk := setup(t)
	g.FailNext(500, 500, 500, 500, 500) // exhausts the idempotent retries
	in := meeting(clk)
	if _, err := c.Schedule(ctx, in); !errors.Is(err, collab.ErrUnavailable) {
		t.Fatalf("outage: %v", err)
	}
	if errors.Is(errFromStatus(t, c, g, 403, clk), collab.ErrAuth) {
		t.Log("a plain 403 is treated as an authorisation problem")
	}
}

func errFromStatus(t *testing.T, c *googlecalendar.Client, g *fakeapi.Google, status int, clk *clock) error {
	t.Helper()
	g.FailNext(status)
	in := meeting(clk)
	in.RequestID = "status-probe"
	_, err := c.Schedule(ctx, in)
	return err
}

func TestCheckVerifiesCredentialsWithoutCreatingAnything(t *testing.T) {
	c, g, _ := setup(t)
	if err := c.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(g.Events()) != 0 {
		t.Fatal("a check must not create events")
	}
	g.Revoke()
	c2, _, _ := setup(t)
	_ = c2
	bad, _ := googlecalendar.New(googlecalendar.Config{ClientID: "client-id", ClientSecret: "nope", RefreshToken: "refresh-1", APIBase: g.URL(), OAuthBase: g.URL()})
	if err := bad.Check(ctx); !errors.Is(err, collab.ErrAuth) {
		t.Fatalf("bad credentials: %v", err)
	}
}
