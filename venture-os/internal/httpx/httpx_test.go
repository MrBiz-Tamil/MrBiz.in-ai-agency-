package httpx_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/httpx"
)

// clock is a fake time source: Sleep advances it instead of waiting, so rate limits and backoff are exact and fast.
type clock struct {
	mu     sync.Mutex
	t      time.Time
	slept  []time.Duration
	failOn func(time.Duration) error
}

func newClock() *clock { return &clock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	return nil
}
func (c *clock) total() (sum time.Duration) {
	for _, d := range c.slept {
		sum += d
	}
	return
}

type scripted struct {
	hits   atomic.Int32
	script []func(w http.ResponseWriter, r *http.Request)
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i := int(s.hits.Add(1)) - 1
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	s.script[i](w, r)
}

func status(code int, hdr ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"code":`+http.StatusText(code)+`}`)
	}
}

func newClient(t *testing.T, h http.Handler, c *clock, tweak func(*httpx.Config)) *httpx.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cfg := httpx.Config{Provider: "fakeco", BaseURL: ts.URL + "/api", Now: c.Now, Sleep: c.Sleep, Rand: func() float64 { return 1 }, BaseBackoff: 100 * time.Millisecond}
	if tweak != nil {
		tweak(&cfg)
	}
	cl, err := httpx.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func TestRetriesServerErrorsForIdempotentRequestsWithBackoff(t *testing.T) {
	c, s := newClock(), &scripted{script: []func(http.ResponseWriter, *http.Request){status(503), status(502), status(200)}}
	cl := newClient(t, s, c, nil)
	r, err := cl.Do(context.Background(), httpx.Request{Path: "/things"})
	if err != nil || r.Status != 200 || s.hits.Load() != 3 {
		t.Fatalf("got %v err=%v hits=%d", r, err, s.hits.Load())
	}
	// Exponential: 100ms then 200ms (jitter fixed at 1.0).
	if len(c.slept) != 2 || c.slept[0] != 100*time.Millisecond || c.slept[1] != 200*time.Millisecond {
		t.Fatalf("backoff = %v", c.slept)
	}
}

func TestDoesNotRetryAPostThatIsNotMarkedIdempotent(t *testing.T) {
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(503), status(200)}}
	cl := newClient(t, s, newClock(), nil)
	_, err := cl.Do(context.Background(), httpx.Request{Method: "POST", Path: "/charge", JSON: map[string]int{"n": 1}})
	if httpx.StatusOf(err) != 503 || s.hits.Load() != 1 {
		t.Fatalf("a non-idempotent POST must not be repeated after a 5xx: err=%v hits=%d", err, s.hits.Load())
	}
	s2 := &scripted{script: []func(http.ResponseWriter, *http.Request){status(503), status(200)}}
	cl2 := newClient(t, s2, newClock(), nil)
	if r, err := cl2.Do(context.Background(), httpx.Request{Method: "POST", Path: "/charge", JSON: 1, Idempotent: true}); err != nil || r.Status != 200 || s2.hits.Load() != 2 {
		t.Fatalf("an idempotent POST is retried: %v %v", r, err)
	}
}

func TestA429IsAlwaysRetriedHonoursRetryAfterAndPausesEveryone(t *testing.T) {
	c := newClock()
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(429, "Retry-After", "2"), status(200)}}
	cl := newClient(t, s, c, nil)
	if r, err := cl.Do(context.Background(), httpx.Request{Method: "POST", Path: "/x", JSON: 1}); err != nil || r.Status != 200 {
		t.Fatalf("a 429 on a POST must be retried: %v %v", r, err)
	}
	if len(c.slept) == 0 || c.slept[0] != 2*time.Second {
		t.Fatalf("Retry-After was not honoured: %v", c.slept)
	}
}

func TestRetryAfterLongerThanWeWillBlockIsReturnedToTheCaller(t *testing.T) {
	c := newClock()
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(429, "Retry-After", "600"), status(200)}}
	cl := newClient(t, s, c, nil)
	_, err := cl.Do(context.Background(), httpx.Request{Path: "/x"})
	var ae *httpx.APIError
	if !errors.As(err, &ae) || ae.Status != 429 || ae.RetryAfter != 600*time.Second || s.hits.Load() != 1 || c.total() != 0 {
		t.Fatalf("err=%v hits=%d slept=%v", err, s.hits.Load(), c.slept)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 422} {
		s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(code), status(200)}}
		cl := newClient(t, s, newClock(), nil)
		_, err := cl.Do(context.Background(), httpx.Request{Path: "/x"})
		if httpx.StatusOf(err) != code || s.hits.Load() != 1 {
			t.Errorf("%d: err=%v hits=%d", code, err, s.hits.Load())
		}
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(500)}}
	cl := newClient(t, s, newClock(), func(c *httpx.Config) { c.MaxRetries = 2; c.BreakerFailures = -1 })
	_, err := cl.Do(context.Background(), httpx.Request{Path: "/x"})
	if httpx.StatusOf(err) != 500 || s.hits.Load() != 3 {
		t.Fatalf("err=%v hits=%d (want 1 try + 2 retries)", err, s.hits.Load())
	}
}

func TestRateLimiterSpacesRequestsWithoutTouchingTheNetworkEarly(t *testing.T) {
	c := newClock()
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(200)}}
	cl := newClient(t, s, c, func(cfg *httpx.Config) { cfg.RatePerSecond = 2; cfg.Burst = 1 })
	for i := 0; i < 5; i++ {
		if _, err := cl.Do(context.Background(), httpx.Request{Path: "/x"}); err != nil {
			t.Fatal(err)
		}
	}
	// 5 requests at 2/s with a burst of 1: the first is free, the other four wait 500 ms each.
	if got := c.total(); got != 2*time.Second {
		t.Fatalf("waited %v, want 2s (%v)", got, c.slept)
	}
}

func TestCircuitOpensAfterRepeatedFailuresThenProbesAfterTheCooldown(t *testing.T) {
	c := newClock()
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(500), status(500), status(500), status(200)}}
	cl := newClient(t, s, c, func(cfg *httpx.Config) {
		cfg.NoRetry = true
		cfg.BreakerFailures = 3
		cfg.BreakerCooldown = time.Minute
	})
	for i := 0; i < 3; i++ {
		_, _ = cl.Do(context.Background(), httpx.Request{Path: "/x"})
	}
	hits := s.hits.Load()
	if _, err := cl.Do(context.Background(), httpx.Request{Path: "/x"}); !errors.Is(err, httpx.ErrCircuitOpen) || s.hits.Load() != hits {
		t.Fatalf("an open circuit must fail fast without calling the provider: %v hits %d->%d", err, hits, s.hits.Load())
	}
	_ = c.Sleep(context.Background(), 2*time.Minute)
	if r, err := cl.Do(context.Background(), httpx.Request{Path: "/x"}); err != nil || r.Status != 200 {
		t.Fatalf("after the cool-down one probe goes through: %v %v", r, err)
	}
	if _, err := cl.Do(context.Background(), httpx.Request{Path: "/x"}); err != nil {
		t.Fatalf("a successful probe closes the circuit: %v", err)
	}
}

func TestClientErrorsDoNotOpenTheCircuit(t *testing.T) {
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(404)}}
	cl := newClient(t, s, newClock(), func(cfg *httpx.Config) { cfg.BreakerFailures = 2 })
	for i := 0; i < 6; i++ {
		if _, err := cl.Do(context.Background(), httpx.Request{Path: "/x"}); errors.Is(err, httpx.ErrCircuitOpen) {
			t.Fatal("404s are the caller's mistake, not the provider failing")
		}
	}
}

func TestRefusesAbsolutePathsAndCrossHostRedirects(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the redirect target was contacted with credentials")
	}))
	defer evil.Close()
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){status(302, "Location", evil.URL+"/steal")}}
	cl := newClient(t, s, newClock(), nil)
	if _, err := cl.Do(context.Background(), httpx.Request{Path: "/x", Header: http.Header{"Authorization": {"Bearer secret"}}}); err == nil {
		t.Fatal("a redirect to another host must fail")
	}
	for _, p := range []string{"http://evil.example/x", "//evil.example/x"} {
		if _, err := cl.Do(context.Background(), httpx.Request{Path: p}); err == nil || !strings.Contains(err.Error(), "relative") {
			t.Errorf("path %q: %v", p, err)
		}
	}
}

func TestErrorsNeverContainCredentialsAndBodiesAreBounded(t *testing.T) {
	big := strings.Repeat("x", 5000)
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); _, _ = io.WriteString(w, big) }}}
	cl := newClient(t, s, newClock(), nil)
	_, err := cl.Do(context.Background(), httpx.Request{Path: "/x", Query: map[string][]string{"access_token": {"TOPSECRET"}}, Header: http.Header{"Authorization": {"Bearer TOPSECRET"}}})
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") || len(err.Error()) > 600 {
		t.Fatalf("error leaks or is unbounded (%d chars): %.80s", len(err.Error()), err)
	}
	// A transport error must not echo the URL (which can carry a token in its query).
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	cl2, _ := httpx.New(httpx.Config{Provider: "fakeco", BaseURL: url, NoRetry: true})
	_, err = cl2.Do(context.Background(), httpx.Request{Path: "/x", Query: map[string][]string{"access_token": {"TOPSECRET"}}})
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("a transport error leaked the query string: %v", err)
	}
}

func TestResponsesOverTheLimitAreRefused(t *testing.T) {
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 2048)) }}}
	cl := newClient(t, s, newClock(), func(c *httpx.Config) { c.MaxResponseBytes = 1024 })
	if _, err := cl.Do(context.Background(), httpx.Request{Path: "/x"}); err == nil {
		t.Fatal("an oversized response was accepted")
	}
}

func TestSendsBodiesAndHeadersAndHonoursCancellation(t *testing.T) {
	var gotBody, gotCT, gotAuth string
	s := &scripted{script: []func(http.ResponseWriter, *http.Request){func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT, gotAuth = string(b), r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}}}
	cl := newClient(t, s, newClock(), nil)
	r, err := cl.Do(context.Background(), httpx.Request{Method: "POST", Path: "api2/x", JSON: map[string]string{"a": "b"}, Header: http.Header{"Authorization": {"Bearer t"}}})
	if err != nil || gotBody != `{"a":"b"}` || gotCT != "application/json" || gotAuth != "Bearer t" {
		t.Fatalf("body=%q ct=%q auth=%q err=%v", gotBody, gotCT, gotAuth, err)
	}
	var out struct{ OK bool }
	if err := r.JSON(&out); err != nil || !out.OK {
		t.Fatal("response JSON")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cl.Do(ctx, httpx.Request{Path: "/x"}); err == nil {
		t.Fatal("a cancelled context must fail the call")
	}
	if _, err := httpx.New(httpx.Config{Provider: "x", BaseURL: "ftp://nope"}); err == nil {
		t.Fatal("a non-http base URL was accepted")
	}
}
