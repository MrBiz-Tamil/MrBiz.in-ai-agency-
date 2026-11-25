package httpx_test

// Regression test for a defect class found by the 2026-11-25 audit of the connectors: url.PathEscape leaves "." and ".." alone,
// so an id that reached a request path unvalidated could address a different endpoint on the provider, with the business's
// credentials. The client now refuses a path with a dot segment, for every connector, whatever its callers checked.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrbiz/venture-os/internal/httpx"
)

func TestPathWithADotSegmentIsRefusedBeforeAnythingIsSent(t *testing.T) {
	var hits atomic.Int32
	cl := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}), newClock(), nil)
	for _, path := range []string{"/events/..", "/events/.", "/a/../b", "../x", "./x", "/a/./b", "events/../../admin"} {
		_, err := cl.Do(context.Background(), httpx.Request{Method: http.MethodDelete, Path: path})
		if err == nil || !strings.Contains(err.Error(), "dot segment") {
			t.Errorf("path %q: want a dot-segment refusal, got %v", path, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused path reached the provider %d time(s)", hits.Load())
	}
	// Dots that are not whole segments are ordinary, and ordinary paths still go through.
	for _, path := range []string{"/files/report.v2.pdf", "/calendars/a.b@group.calendar.google.com/events", "/..hidden/ok", "/v1/users/me"} {
		if _, err := cl.Do(context.Background(), httpx.Request{Path: path}); err != nil {
			t.Errorf("path %q should be allowed: %v", path, err)
		}
	}
}
