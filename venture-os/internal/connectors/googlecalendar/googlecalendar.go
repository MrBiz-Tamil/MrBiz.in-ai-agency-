// Package googlecalendar implements collab.Calendar on the Google Calendar API v3. Meetings are calendar events with a
// Google Meet link attached (conferenceData with a hangoutsMeet create request), so one adapter covers both
// "Google Calendar" and "Google Meet".
//
// Authentication is an OAuth 2.0 refresh token obtained when the business connected its Google account: the adapter
// exchanges it for short-lived access tokens, caches them until shortly before they expire, and renews on a 401. A
// refresh token Google has revoked surfaces as collab.ErrAuth, meaning a human must reconnect.
//
// Idempotency: when the caller supplies a RequestID, the event id is derived from it (Google accepts a client-chosen id
// of base32hex characters), so a retried create cannot make a second event: Google answers 409 and the adapter returns
// the event that already exists.
//
// Invitations: Notify maps to sendUpdates=all, which makes Google email every attendee. That is outward-facing, which
// is why the agent tools put it behind founder approval.
package googlecalendar

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/httpx"
)

const (
	defaultAPI   = "https://www.googleapis.com/calendar/v3"
	defaultOAuth = "https://oauth2.googleapis.com"
)

// Config holds one business's Google connection.
type Config struct {
	ClientID, ClientSecret, RefreshToken string
	CalendarID                           string // default "primary"

	// Overrides, for tests: base URLs, a tuning hook for the HTTP clients, and the clock.
	APIBase, OAuthBase string
	Tune               func(*httpx.Config)
	Now                func() time.Time
	Sleep              func(ctx context.Context, d time.Duration) error
}

// Client is a connected Google Calendar. Safe for concurrent use.
type Client struct {
	cfg   Config
	api   *httpx.Client
	oauth *httpx.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

var _ collab.Calendar = (*Client)(nil)

// New builds a client; it does not call Google.
func New(cfg Config) (*Client, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RefreshToken == "" {
		return nil, fmt.Errorf("%w: google needs a client id, client secret and refresh token", collab.ErrInvalid)
	}
	if cfg.CalendarID == "" {
		cfg.CalendarID = "primary"
	}
	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPI
	}
	if cfg.OAuthBase == "" {
		cfg.OAuthBase = defaultOAuth
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
	mk := func(base string, rate float64) (*httpx.Client, error) {
		hc := httpx.Config{Provider: "google", BaseURL: base, RatePerSecond: rate, Burst: 5, Now: cfg.Now}
		if cfg.Tune != nil {
			cfg.Tune(&hc)
		}
		return httpx.New(hc)
	}
	api, err := mk(cfg.APIBase, 8) // Calendar's per-user quota is generous; this keeps a runaway loop polite
	if err != nil {
		return nil, err
	}
	oauth, err := mk(cfg.OAuthBase, 2)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, api: api, oauth: oauth}, nil
}

// ───────────────────────────── authentication ─────────────────────────────

func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.cfg.Now().Before(c.expires.Add(-time.Minute)) {
		return c.token, nil
	}
	resp, err := c.oauth.Do(ctx, httpx.Request{
		Method: http.MethodPost, Path: "/token",
		Form: url.Values{"client_id": {c.cfg.ClientID}, "client_secret": {c.cfg.ClientSecret}, "refresh_token": {c.cfg.RefreshToken}, "grant_type": {"refresh_token"}},
	})
	if err != nil {
		if s := httpx.StatusOf(err); s == http.StatusBadRequest || s == http.StatusUnauthorized {
			// invalid_grant (revoked or expired refresh token) and invalid_client (wrong secret) both mean: reconnect.
			return "", fmt.Errorf("%w: Google refused the refresh token; reconnect the Google account", collab.ErrAuth)
		}
		return "", mapErr(err)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := resp.JSON(&tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("%w: Google returned no access token", collab.ErrUnavailable)
	}
	c.token, c.expires = tr.AccessToken, c.cfg.Now().Add(time.Duration(tr.ExpiresIn)*time.Second)
	return c.token, nil
}

// call sends an authenticated request, renewing the access token once if Google says it is no longer valid.
func (c *Client) call(ctx context.Context, r httpx.Request) (*httpx.Response, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		r.Header = http.Header{"Authorization": {"Bearer " + tok}}
		resp, err := c.api.Do(ctx, r)
		if err != nil && httpx.StatusOf(err) == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		return resp, mapErr(err)
	}
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ae *httpx.APIError
	switch {
	case errors.Is(err, httpx.ErrCircuitOpen), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", collab.ErrUnavailable, err)
	case !errors.As(err, &ae):
		return fmt.Errorf("%w: %v", collab.ErrUnavailable, err)
	}
	switch {
	case ae.Status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %w", collab.ErrAuth, ae)
	case ae.Status == http.StatusForbidden && strings.Contains(ae.Body, "ateLimitExceeded"):
		return fmt.Errorf("%w: Google rate limit: %w", collab.ErrUnavailable, ae) // Google signals quota with a 403
	case ae.Status == http.StatusForbidden:
		return fmt.Errorf("%w: %w", collab.ErrAuth, ae)
	case ae.Status == http.StatusNotFound || ae.Status == http.StatusGone:
		return fmt.Errorf("%w: %w", collab.ErrNotFound, ae)
	case ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests:
		return fmt.Errorf("%w: %w", collab.ErrInvalid, ae)
	}
	return fmt.Errorf("%w: %w", collab.ErrUnavailable, ae)
}

// Check verifies the credentials by exchanging the refresh token and reading the calendar's availability for a
// moment; it creates and changes nothing.
func (c *Client) Check(ctx context.Context) error {
	if _, err := c.accessToken(ctx, true); err != nil {
		return err
	}
	now := c.cfg.Now()
	_, err := c.FreeBusy(ctx, now, now.Add(time.Hour))
	return err
}

// ───────────────────────────── events ─────────────────────────────

type dateTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type event struct {
	ID          string     `json:"id,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status,omitempty"`
	HangoutLink string     `json:"hangoutLink,omitempty"`
	Start       *dateTime  `json:"start,omitempty"`
	End         *dateTime  `json:"end,omitempty"`
	Attendees   []attendee `json:"attendees,omitempty"`
	Conference  *conf      `json:"conferenceData,omitempty"`
}

type attendee struct {
	Email string `json:"email"`
}

type conf struct {
	CreateRequest *struct {
		RequestID             string `json:"requestId,omitempty"`
		ConferenceSolutionKey *struct {
			Type string `json:"type"`
		} `json:"conferenceSolutionKey,omitempty"`
		Status *struct {
			StatusCode string `json:"statusCode"`
		} `json:"status,omitempty"`
	} `json:"createRequest,omitempty"`
	EntryPoints []struct {
		EntryPointType string `json:"entryPointType"`
		URI            string `json:"uri"`
	} `json:"entryPoints,omitempty"`
}

var eventIDEncoding = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

// EventID derives Google's client-chosen event id from a request id: 26 characters of [0-9a-v], within Google's
// allowed alphabet and length (5 to 1024).
func EventID(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return eventIDEncoding.EncodeToString(sum[:16])
}

func (c *Client) eventPath(id string) string {
	return "/calendars/" + url.PathEscape(c.cfg.CalendarID) + "/events/" + url.PathEscape(id)
}

func toMeeting(e event, created bool) collab.Meeting {
	m := collab.Meeting{ID: e.ID, Provider: "google", Title: e.Summary, JoinURL: e.HangoutLink, Created: created}
	if m.JoinURL == "" && e.Conference != nil {
		for _, p := range e.Conference.EntryPoints {
			if p.EntryPointType == "video" {
				m.JoinURL = p.URI
				break
			}
		}
	}
	if e.Start != nil {
		m.Start, _ = time.Parse(time.RFC3339, e.Start.DateTime)
	}
	if e.End != nil {
		m.End, _ = time.Parse(time.RFC3339, e.End.DateTime)
	}
	for _, a := range e.Attendees {
		m.Attendees = append(m.Attendees, a.Email)
	}
	return m
}

// Schedule creates the event with a Meet link. With a RequestID it is idempotent.
func (c *Client) Schedule(ctx context.Context, in collab.MeetingInput) (collab.Meeting, error) {
	in, err := collab.NormalizeMeeting(in, c.cfg.Now())
	if err != nil {
		return collab.Meeting{}, err
	}
	ev := event{
		Summary: in.Title, Description: in.Description,
		Start: &dateTime{DateTime: in.Start.Format(time.RFC3339), TimeZone: in.Timezone},
		End:   &dateTime{DateTime: in.Start.Add(in.Duration).Format(time.RFC3339), TimeZone: in.Timezone},
		Conference: &conf{CreateRequest: &struct {
			RequestID             string `json:"requestId,omitempty"`
			ConferenceSolutionKey *struct {
				Type string `json:"type"`
			} `json:"conferenceSolutionKey,omitempty"`
			Status *struct {
				StatusCode string `json:"statusCode"`
			} `json:"status,omitempty"`
		}{RequestID: firstNonEmpty(in.RequestID, fmt.Sprintf("req-%d", c.cfg.Now().UnixNano())), ConferenceSolutionKey: &struct {
			Type string `json:"type"`
		}{Type: "hangoutsMeet"}}},
	}
	for _, a := range in.Attendees {
		ev.Attendees = append(ev.Attendees, attendee{Email: a})
	}
	if in.RequestID != "" {
		ev.ID = EventID(in.RequestID)
	}
	notify := "none"
	if in.Notify && len(in.Attendees) > 0 {
		notify = "all"
	}
	resp, err := c.call(ctx, httpx.Request{
		Method: http.MethodPost, Path: "/calendars/" + url.PathEscape(c.cfg.CalendarID) + "/events",
		Query:      url.Values{"conferenceDataVersion": {"1"}, "sendUpdates": {notify}},
		JSON:       ev,
		Idempotent: ev.ID != "", // a client-chosen id makes a repeat harmless: Google answers 409
	})
	if err != nil {
		if ev.ID != "" && httpx.StatusOf(err) == http.StatusConflict {
			existing, gerr := c.Get(ctx, ev.ID) // an earlier attempt already created it
			existing.Created = false
			return existing, gerr
		}
		return collab.Meeting{}, err
	}
	var created event
	if err := resp.JSON(&created); err != nil {
		return collab.Meeting{}, fmt.Errorf("%w: unreadable event", collab.ErrUnavailable)
	}
	// The Meet link is created asynchronously; it is normally there already, but give Google a moment if not.
	for polls := 0; created.HangoutLink == "" && pending(created) && polls < 3; polls++ {
		if err := c.cfg.Sleep(ctx, 400*time.Millisecond); err != nil {
			return collab.Meeting{}, err
		}
		again, err := c.Get(ctx, created.ID)
		if err != nil {
			return collab.Meeting{}, err
		}
		created.HangoutLink = again.JoinURL
		if again.JoinURL != "" {
			created.Conference = nil
		}
	}
	return toMeeting(created, true), nil
}

func pending(e event) bool {
	return e.Conference != nil && e.Conference.CreateRequest != nil && e.Conference.CreateRequest.Status != nil &&
		e.Conference.CreateRequest.Status.StatusCode == "pending"
}

// eventIDRe is the alphabet of the event ids this adapter creates (base32hex) and of the ids Google returns, including a
// recurring instance's "<id>_<20260101T100000Z>". It deliberately excludes ".", "/" and anything else with meaning in a
// URL path: the id comes from an agent, and url.PathEscape leaves "." and ".." alone, which a server reads as "the parent
// directory" (so DELETE .../events/.. would address the calendar itself).
var eventIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validEventID(id string) error {
	if len(id) > 1024 || !eventIDRe.MatchString(id) { // Google allows up to 1,024 characters (RE2 caps a repeat count at 1,000)
		return fmt.Errorf("%w: %q is not a Google Calendar event id", collab.ErrInvalid, id)
	}
	return nil
}

// Get reads an event.
func (c *Client) Get(ctx context.Context, id string) (collab.Meeting, error) {
	if err := validEventID(id); err != nil {
		return collab.Meeting{}, err
	}
	resp, err := c.call(ctx, httpx.Request{Path: c.eventPath(id)})
	if err != nil {
		return collab.Meeting{}, err
	}
	var e event
	if err := resp.JSON(&e); err != nil {
		return collab.Meeting{}, fmt.Errorf("%w: unreadable event", collab.ErrUnavailable)
	}
	if e.Status == "cancelled" {
		return collab.Meeting{}, fmt.Errorf("%w: the event was cancelled", collab.ErrNotFound)
	}
	return toMeeting(e, false), nil
}

// Cancel deletes the event. notify controls whether Google emails the attendees. An absent event counts as done.
func (c *Client) Cancel(ctx context.Context, id string, notify bool) error {
	if err := validEventID(id); err != nil {
		return err
	}
	send := "none"
	if notify {
		send = "all"
	}
	_, err := c.call(ctx, httpx.Request{Method: http.MethodDelete, Path: c.eventPath(id), Query: url.Values{"sendUpdates": {send}}})
	if errors.Is(err, collab.ErrNotFound) {
		return nil
	}
	return err
}

// FreeBusy lists the busy periods of the calendar between from and to.
func (c *Client) FreeBusy(ctx context.Context, from, to time.Time) ([]collab.Interval, error) {
	if !to.After(from) || to.Sub(from) > 62*24*time.Hour {
		return nil, fmt.Errorf("%w: the window must be positive and at most 62 days", collab.ErrInvalid)
	}
	resp, err := c.call(ctx, httpx.Request{Method: http.MethodPost, Path: "/freeBusy", Idempotent: true, JSON: map[string]any{
		"timeMin": from.UTC().Format(time.RFC3339), "timeMax": to.UTC().Format(time.RFC3339),
		"items": []map[string]string{{"id": c.cfg.CalendarID}},
	}})
	if err != nil {
		return nil, err
	}
	var fb struct {
		Calendars map[string]struct {
			Busy   []struct{ Start, End string } `json:"busy"`
			Errors []struct{ Reason string }     `json:"errors"`
		} `json:"calendars"`
	}
	if err := resp.JSON(&fb); err != nil {
		return nil, fmt.Errorf("%w: unreadable free/busy answer", collab.ErrUnavailable)
	}
	cal, ok := fb.Calendars[c.cfg.CalendarID]
	if !ok && len(fb.Calendars) == 1 { // Google may key "primary" by the account's address
		for _, only := range fb.Calendars {
			cal, ok = only, true
		}
	}
	if !ok || len(cal.Errors) > 0 {
		return nil, fmt.Errorf("%w: Google could not read availability for %q", collab.ErrNotFound, c.cfg.CalendarID)
	}
	out := make([]collab.Interval, 0, len(cal.Busy))
	for _, b := range cal.Busy {
		s, e1 := time.Parse(time.RFC3339, b.Start)
		e, e2 := time.Parse(time.RFC3339, b.End)
		if e1 == nil && e2 == nil {
			out = append(out, collab.Interval{Start: s.UTC(), End: e.UTC()})
		}
	}
	return out, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
