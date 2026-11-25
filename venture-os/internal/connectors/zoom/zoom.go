// Package zoom implements collab.Meetings on the Zoom Meetings API v2 using Server-to-Server OAuth: the business
// creates a Server-to-Server OAuth app in its Zoom account and gives us the account id, client id and client secret;
// the adapter exchanges them for short-lived access tokens (grant_type=account_credentials), caches them until shortly
// before expiry, and renews on a 401. Rejected credentials surface as collab.ErrAuth.
//
// What it does NOT do: Zoom's API does not email invitations. Attendees are recorded as the meeting's invitees (so
// they can join a meeting that requires authentication) and the join link is returned; delivering the link is a
// separate, approval-gated step. There is also no idempotency key on meeting creation, so a create whose response was
// lost (a timeout after Zoom accepted it) is NOT retried blindly: it surfaces as an error and a human may find a
// duplicate meeting in Zoom.
package zoom

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/httpx"
)

const (
	defaultAPI   = "https://api.zoom.us/v2"
	defaultOAuth = "https://zoom.us"
)

// Config holds one business's Zoom connection.
type Config struct {
	AccountID, ClientID, ClientSecret string
	UserID                            string // whose meetings to create; default "me"

	// Overrides, for tests.
	APIBase, OAuthBase string
	Tune               func(*httpx.Config)
	Now                func() time.Time
}

// Client is a connected Zoom account. Safe for concurrent use.
type Client struct {
	cfg   Config
	api   *httpx.Client
	oauth *httpx.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

var _ collab.Meetings = (*Client)(nil)

// New builds a client; it does not call Zoom.
func New(cfg Config) (*Client, error) {
	if cfg.AccountID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("%w: zoom needs an account id, client id and client secret", collab.ErrInvalid)
	}
	if cfg.UserID == "" {
		cfg.UserID = "me"
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
	mk := func(base string, rate float64) (*httpx.Client, error) {
		hc := httpx.Config{Provider: "zoom", BaseURL: base, RatePerSecond: rate, Burst: 5, Now: cfg.Now}
		if cfg.Tune != nil {
			cfg.Tune(&hc)
		}
		return httpx.New(hc)
	}
	api, err := mk(cfg.APIBase, 8) // Zoom's limits are per account and per endpoint class; stay well under them
	if err != nil {
		return nil, err
	}
	oauth, err := mk(cfg.OAuthBase, 2)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, api: api, oauth: oauth}, nil
}

func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.cfg.Now().Before(c.expires.Add(-time.Minute)) {
		return c.token, nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte(c.cfg.ClientID + ":" + c.cfg.ClientSecret))
	resp, err := c.oauth.Do(ctx, httpx.Request{
		Method: http.MethodPost, Path: "/oauth/token",
		Query:  url.Values{"grant_type": {"account_credentials"}, "account_id": {c.cfg.AccountID}},
		Header: http.Header{"Authorization": {"Basic " + basic}},
	})
	if err != nil {
		if s := httpx.StatusOf(err); s == http.StatusBadRequest || s == http.StatusUnauthorized {
			return "", fmt.Errorf("%w: Zoom refused the account credentials; check the Server-to-Server OAuth app", collab.ErrAuth)
		}
		return "", mapErr(err)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := resp.JSON(&tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("%w: Zoom returned no access token", collab.ErrUnavailable)
	}
	c.token, c.expires = tr.AccessToken, c.cfg.Now().Add(time.Duration(tr.ExpiresIn)*time.Second)
	return c.token, nil
}

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
	case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
		return fmt.Errorf("%w: %w", collab.ErrAuth, ae)
	case ae.Status == http.StatusNotFound:
		return fmt.Errorf("%w: %w", collab.ErrNotFound, ae)
	case ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests:
		return fmt.Errorf("%w: %w", collab.ErrInvalid, ae)
	}
	return fmt.Errorf("%w: %w", collab.ErrUnavailable, ae)
}

// Check verifies the credentials by obtaining an access token; it creates nothing.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.accessToken(ctx, true)
	return err
}

type meeting struct {
	ID        json64   `json:"id"`
	Topic     string   `json:"topic"`
	StartTime string   `json:"start_time"`
	Duration  int      `json:"duration"`
	JoinURL   string   `json:"join_url"`
	Settings  settings `json:"settings"`
}

type settings struct {
	JoinBeforeHost  bool      `json:"join_before_host"`
	WaitingRoom     bool      `json:"waiting_room"`
	MeetingInvitees []invitee `json:"meeting_invitees,omitempty"`
}

type invitee struct {
	Email string `json:"email"`
}

// json64 reads Zoom's meeting id, which is a JSON number that can exceed what a float holds exactly.
type json64 string

func (j *json64) UnmarshalJSON(b []byte) error {
	*j = json64(strings.Trim(string(b), `"`))
	return nil
}

func toMeeting(m meeting, created bool) collab.Meeting {
	out := collab.Meeting{ID: string(m.ID), Provider: "zoom", Title: m.Topic, JoinURL: m.JoinURL, Created: created}
	if t, err := time.Parse(time.RFC3339, m.StartTime); err == nil {
		out.Start = t.UTC()
		out.End = out.Start.Add(time.Duration(m.Duration) * time.Minute)
	}
	for _, i := range m.Settings.MeetingInvitees {
		out.Attendees = append(out.Attendees, i.Email)
	}
	return out
}

func validID(id string) error {
	if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
		return fmt.Errorf("%w: %q is not a Zoom meeting id", collab.ErrInvalid, id)
	}
	return nil
}

// Schedule creates a scheduled meeting (type 2) with a waiting room, and returns its join link.
func (c *Client) Schedule(ctx context.Context, in collab.MeetingInput) (collab.Meeting, error) {
	in, err := collab.NormalizeMeeting(in, c.cfg.Now())
	if err != nil {
		return collab.Meeting{}, err
	}
	body := map[string]any{
		"topic": in.Title, "type": 2, "start_time": in.Start.Format(time.RFC3339), "duration": int(in.Duration.Minutes()),
		"timezone": in.Timezone, "agenda": in.Description,
		"settings": settings{JoinBeforeHost: false, WaitingRoom: true, MeetingInvitees: toInvitees(in.Attendees)},
	}
	resp, err := c.call(ctx, httpx.Request{Method: http.MethodPost, Path: "/users/" + url.PathEscape(c.cfg.UserID) + "/meetings", JSON: body})
	if err != nil {
		return collab.Meeting{}, err
	}
	var m meeting
	if err := resp.JSON(&m); err != nil || m.ID == "" || m.JoinURL == "" {
		return collab.Meeting{}, fmt.Errorf("%w: Zoom returned an unreadable meeting", collab.ErrUnavailable)
	}
	out := toMeeting(m, true)
	if out.Start.IsZero() {
		out.Start, out.End = in.Start, in.Start.Add(in.Duration)
	}
	if len(out.Attendees) == 0 {
		out.Attendees = in.Attendees
	}
	return out, nil
}

func toInvitees(emails []string) []invitee {
	var out []invitee
	for _, e := range emails {
		out = append(out, invitee{Email: e})
	}
	return out
}

// Get reads a meeting.
func (c *Client) Get(ctx context.Context, id string) (collab.Meeting, error) {
	if err := validID(id); err != nil {
		return collab.Meeting{}, err
	}
	resp, err := c.call(ctx, httpx.Request{Path: "/meetings/" + id})
	if err != nil {
		return collab.Meeting{}, err
	}
	var m meeting
	if err := resp.JSON(&m); err != nil {
		return collab.Meeting{}, fmt.Errorf("%w: unreadable meeting", collab.ErrUnavailable)
	}
	return toMeeting(m, false), nil
}

// Cancel deletes the meeting. notify asks Zoom to send cancellation reminders. An absent meeting counts as done.
func (c *Client) Cancel(ctx context.Context, id string, notify bool) error {
	if err := validID(id); err != nil {
		return err
	}
	_, err := c.call(ctx, httpx.Request{Method: http.MethodDelete, Path: "/meetings/" + id, Query: url.Values{"cancel_meeting_reminder": {strconv.FormatBool(notify)}}})
	if errors.Is(err, collab.ErrNotFound) {
		return nil
	}
	return err
}
