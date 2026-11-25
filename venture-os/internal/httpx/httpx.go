// Package httpx is the HTTP client every third-party connector uses, so reliability is built once and behaves the
// same everywhere:
//
//   - A token-bucket rate limiter per client (that is, per provider), because every SaaS API throttles and the
//     polite way to cope is to not exceed the limit in the first place.
//   - Retries with exponential backoff and full jitter, honouring Retry-After. A request is retried only when that is
//     safe: network errors and 5xx for idempotent methods (or requests the caller marks Idempotent), and 429 for
//     everything, since a 429 means the provider did not process the request.
//   - A shared pause: when a provider says "slow down", every goroutine using the client waits, not just the one
//     that was told.
//   - A circuit breaker: after repeated failures the client fails fast for a cool-down instead of piling more
//     requests onto a struggling provider.
//   - Bounded responses and errors that never contain credentials: error text carries the provider, status and a short
//     body excerpt, never request headers.
//   - It refuses redirects to another host, so a bearer token cannot be forwarded to a third party.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config configures a Client. Zero values pick safe defaults.
type Config struct {
	Provider string // names the provider in errors, e.g. "zoom"
	BaseURL  string // scheme://host[/prefix]; request paths are appended to it

	HTTP *http.Client // default: 30 s timeout

	RatePerSecond float64 // sustained requests per second; 0 = unlimited
	Burst         int     // bucket size; default 1 when RatePerSecond is set

	MaxRetries  int           // default 3 (0 means the default; use NoRetry to disable)
	NoRetry     bool          // never retry
	BaseBackoff time.Duration // default 250 ms
	MaxBackoff  time.Duration // default 8 s: also the longest Retry-After the client will wait out

	BreakerFailures int           // consecutive failures that open the circuit; default 8, <0 disables
	BreakerCooldown time.Duration // default 30 s

	MaxResponseBytes int64 // default 4 MiB

	// Test seams.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Rand  func() float64
}

// Request is one call.
type Request struct {
	Method string // default GET
	Path   string // relative to BaseURL; must not be an absolute URL
	Query  url.Values
	Header http.Header

	// At most one body. JSON is marshalled with Content-Type application/json; Form is urlencoded.
	JSON any
	Form url.Values
	Body []byte

	// Idempotent says a POST/PATCH is safe to repeat (for example it carries an idempotency key or a request id), so
	// it may be retried after a 5xx or a network error.
	Idempotent bool
}

// Response is a completed call with a status below 400.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON decodes the body into v.
func (r *Response) JSON(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// APIError is a failed call. Body is a short excerpt for diagnosis and is not guaranteed to be JSON.
type APIError struct {
	Provider   string
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Body)
}

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// ErrCircuitOpen means the provider has been failing and calls are being refused for a cool-down.
var ErrCircuitOpen = errors.New("httpx: circuit open, the provider is failing; try again shortly")

// Client is safe for concurrent use.
type Client struct {
	cfg  Config
	base *url.URL
	http *http.Client

	mu       sync.Mutex
	tokens   float64
	lastFill time.Time
	pause    time.Time // nobody sends before this instant

	failures int
	openedAt time.Time
}

// New builds a client. It fails on an unusable BaseURL.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("httpx: %s: base URL %q is not an http(s) URL", cfg.Provider, cfg.BaseURL)
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.NoRetry {
		cfg.MaxRetries = 0
	}
	if cfg.BaseBackoff == 0 {
		cfg.BaseBackoff = 250 * time.Millisecond
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 8 * time.Second
	}
	if cfg.BreakerFailures == 0 {
		cfg.BreakerFailures = 8
	}
	if cfg.BreakerCooldown == 0 {
		cfg.BreakerCooldown = 30 * time.Second
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 4 << 20
	}
	if cfg.RatePerSecond > 0 && cfg.Burst < 1 {
		cfg.Burst = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// Never follow a redirect to another host: it would carry our Authorization header with it.
	hcCopy := *hc
	prev := hc.CheckRedirect
	hcCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("httpx: %s: refusing redirect to another host", cfg.Provider)
		}
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 5 {
			return errors.New("httpx: too many redirects")
		}
		return nil
	}
	return &Client{cfg: cfg, base: u, http: &hcCopy, tokens: float64(cfg.Burst), lastFill: cfg.Now()}, nil
}

func isIdempotentMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

func (c *Client) buildURL(r Request) (string, error) {
	if strings.Contains(r.Path, "://") || strings.HasPrefix(r.Path, "//") {
		return "", fmt.Errorf("httpx: %s: path %q must be relative", c.cfg.Provider, r.Path)
	}
	// A "." or ".." segment is read by a server as "this" or "the parent directory", so an id that reached a path unescaped
	// (url.PathEscape leaves dots alone) could redirect the call to another endpoint with this provider's credentials. No
	// legitimate request has one, so refuse it here for every connector, whatever its callers validated.
	for _, seg := range strings.Split(r.Path, "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("httpx: %s: path %q contains a dot segment", c.cfg.Provider, r.Path)
		}
	}
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + "/" + strings.TrimLeft(r.Path, "/")
	if r.Query != nil {
		u.RawQuery = r.Query.Encode()
	}
	return u.String(), nil
}

func (c *Client) body(r Request) ([]byte, string, error) {
	switch {
	case r.JSON != nil:
		b, err := json.Marshal(r.JSON)
		return b, "application/json", err
	case r.Form != nil:
		return []byte(r.Form.Encode()), "application/x-www-form-urlencoded", nil
	}
	return r.Body, "", nil
}

// wait blocks until a token is available and any shared pause has passed.
func (c *Client) wait(ctx context.Context) error {
	for {
		c.mu.Lock()
		now := c.cfg.Now()
		var d time.Duration
		if c.pause.After(now) {
			d = c.pause.Sub(now)
		} else if c.cfg.RatePerSecond > 0 {
			c.tokens = math.Min(float64(c.cfg.Burst), c.tokens+now.Sub(c.lastFill).Seconds()*c.cfg.RatePerSecond)
			c.lastFill = now
			if c.tokens >= 1 {
				c.tokens--
			} else {
				d = time.Duration((1 - c.tokens) / c.cfg.RatePerSecond * float64(time.Second))
			}
		}
		c.mu.Unlock()
		if d <= 0 {
			return nil
		}
		if err := c.cfg.Sleep(ctx, d); err != nil {
			return err
		}
	}
}

func (c *Client) pauseFor(d time.Duration) {
	c.mu.Lock()
	if until := c.cfg.Now().Add(d); until.After(c.pause) {
		c.pause = until
	}
	c.mu.Unlock()
}

func (c *Client) circuitOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg.BreakerFailures < 0 || c.failures < c.cfg.BreakerFailures {
		return false
	}
	if c.cfg.Now().Sub(c.openedAt) >= c.cfg.BreakerCooldown {
		c.failures = c.cfg.BreakerFailures - 1 // half-open: one probe decides
		return false
	}
	return true
}

func (c *Client) record(ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.failures = 0
		return
	}
	c.failures++
	if c.failures >= c.cfg.BreakerFailures {
		c.openedAt = c.cfg.Now()
	}
}

func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func (c *Client) backoff(attempt int) time.Duration {
	d := float64(c.cfg.BaseBackoff) * math.Pow(2, float64(attempt))
	if d > float64(c.cfg.MaxBackoff) {
		d = float64(c.cfg.MaxBackoff)
	}
	return time.Duration(d * (0.5 + c.cfg.Rand()/2)) // jitter between 50% and 100%
}

// Do sends the request, applying the rate limit, retries and circuit breaker. A status of 400 or above is an
// *APIError (after retries where they apply).
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	full, err := c.buildURL(r)
	if err != nil {
		return nil, err
	}
	payload, ctype, err := c.body(r)
	if err != nil {
		return nil, fmt.Errorf("httpx: %s: encode request: %w", c.cfg.Provider, err)
	}
	retrySafe := isIdempotentMethod(r.Method) || r.Idempotent

	var lastErr error
	for attempt := 0; ; attempt++ {
		if c.circuitOpen() {
			return nil, fmt.Errorf("%s: %w", c.cfg.Provider, ErrCircuitOpen)
		}
		if err := c.wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, r.Method, full, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		for k, vs := range r.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if ctype != "" && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", ctype)
		}
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/json")
		}

		resp, err := c.http.Do(req)
		var (
			status int
			hdr    http.Header
			body   []byte
		)
		if err == nil {
			status, hdr = resp.StatusCode, resp.Header
			body, err = io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
			resp.Body.Close()
			if err == nil && int64(len(body)) > c.cfg.MaxResponseBytes {
				return nil, fmt.Errorf("httpx: %s: response larger than %d bytes", c.cfg.Provider, c.cfg.MaxResponseBytes)
			}
		}

		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.record(false)
			lastErr = fmt.Errorf("%s: %w", c.cfg.Provider, scrub(err))
			if !retrySafe || attempt >= c.cfg.MaxRetries {
				return nil, lastErr
			}
			if err := c.cfg.Sleep(ctx, c.backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		case status < 400:
			c.record(true)
			return &Response{Status: status, Header: hdr, Body: body}, nil
		}

		ae := &APIError{Provider: c.cfg.Provider, Status: status, Body: excerpt(body), RetryAfter: retryAfter(hdr, c.cfg.Now())}
		lastErr = ae
		serverSide := status >= 500
		c.record(!serverSide && status != http.StatusTooManyRequests)

		canRetry := attempt < c.cfg.MaxRetries && (status == http.StatusTooManyRequests || (serverSide && retrySafe))
		if !canRetry {
			return nil, ae
		}
		wait := c.backoff(attempt)
		if ae.RetryAfter > 0 {
			if ae.RetryAfter > c.cfg.MaxBackoff {
				return nil, ae // the provider wants a longer wait than we will block for; let the caller decide
			}
			wait = ae.RetryAfter
		}
		if status == http.StatusTooManyRequests {
			c.pauseFor(wait) // everyone slows down, not just this caller
		}
		if err := c.cfg.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// scrub drops the URL from transport errors (net/http includes it, and URLs can carry tokens in their query).
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
