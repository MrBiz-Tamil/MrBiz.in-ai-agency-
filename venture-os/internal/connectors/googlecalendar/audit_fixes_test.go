package googlecalendar_test

// Regression tests for a defect found by the 2026-11-25 audit of the connectors. meeting_get and meeting_cancel take the event id
// from an AGENT. url.PathEscape leaves "." and ".." alone, so an id of ".." made the adapter send DELETE .../events/.., which
// a server resolves to the calendar itself. The id is now validated before any request is built. Each failed before the fix.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/connectors/googlecalendar"
	"github.com/mrbiz/venture-os/internal/httpx"
)

// recorder is a Google API stand-in that answers every call and remembers the raw request line of each.
func recorder(t *testing.T) (*googlecalendar.Client, *[]string) {
	t.Helper()
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "token") {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"token_type":"Bearer"}`))
			return
		}
		uris = append(uris, r.Method+" "+r.RequestURI)
		_, _ = w.Write([]byte(`{"id":"x","status":"confirmed","summary":"s","start":{"dateTime":"2026-03-02T09:00:00Z"},"end":{"dateTime":"2026-03-02T10:00:00Z"}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := googlecalendar.New(googlecalendar.Config{ClientID: "i", ClientSecret: "s", RefreshToken: "r", APIBase: srv.URL, OAuthBase: srv.URL,
		Tune: func(h *httpx.Config) {
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			h.Rand = func() float64 { return 1 }
		}})
	if err != nil {
		t.Fatal(err)
	}
	return c, &uris
}

func TestEventIDCannotBeADotSegmentOrChangeThePath(t *testing.T) {
	c, uris := recorder(t)
	for _, id := range []string{"..", ".", "../..", "a/../b", "a/b", "a?x=1", "a#f", "a b", "evt%2f..%2f", "", strings.Repeat("a", 1025)} {
		for name, call := range map[string]func() error{
			"Get":    func() error { _, err := c.Get(ctx, id); return err },
			"Cancel": func() error { return c.Cancel(ctx, id, false) },
		} {
			if err := call(); !errors.Is(err, collab.ErrInvalid) {
				t.Errorf("%s(%q): want ErrInvalid, got %v", name, id, err)
			}
		}
	}
	if len(*uris) != 0 {
		t.Fatalf("a refused id must not reach the provider, but these were sent: %v", *uris)
	}
}

func TestRealEventIDsStillWork(t *testing.T) {
	c, uris := recorder(t)
	for _, id := range []string{
		"0123456789abcdefghijklmnopqrstuv",    // base32hex, the alphabet of ids this adapter creates
		"7e9n4s2i1g3d8q6r0t5uvabcde",          // a Google-generated id
		"abc123_20260301T090000Z",             // a recurring instance
		"Event_ID-with-Mixed-Case_and-dashes", // other legal characters
	} {
		if _, err := c.Get(ctx, id); err != nil {
			t.Errorf("Get(%q): %v", id, err)
		}
		if err := c.Cancel(ctx, id, false); err != nil {
			t.Errorf("Cancel(%q): %v", id, err)
		}
	}
	if len(*uris) != 8 {
		t.Fatalf("expected 8 requests (a Get and a Cancel for each of 4 ids), got %d: %v", len(*uris), *uris)
	}
	for _, u := range *uris {
		if !strings.Contains(u, "/calendars/primary/events/") {
			t.Errorf("unexpected request %q", u)
		}
	}
}
