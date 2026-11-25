// Package collab defines the provider-neutral interfaces for meetings, calendars and notes, and the types they share.
// Adapters live in internal/connectors (googlecalendar, zoom, notion); the agent tools in internal/collabops talk only
// to these interfaces, so adding a provider (Microsoft Teams, Outlook, Confluence) is an adapter, not a rewrite.
package collab

import (
	"context"
	"errors"
	"time"
)

// Errors every adapter returns (wrapped with detail), so callers can react without knowing the provider.
var (
	// ErrNotConnected: the business has not connected this provider.
	ErrNotConnected = errors.New("collab: provider not connected")
	// ErrAuth: the provider refused our credentials (revoked, expired refresh token, wrong secret). A human must
	// reconnect; retrying cannot help.
	ErrAuth = errors.New("collab: provider rejected the credentials")
	// ErrNotFound: the meeting or page does not exist (or is not visible to the integration).
	ErrNotFound = errors.New("collab: not found")
	// ErrInvalid: the input breaks a rule or the provider refused it as malformed.
	ErrInvalid = errors.New("collab: invalid input")
	// ErrUnavailable: the provider is failing or rate limiting; retry later.
	ErrUnavailable = errors.New("collab: provider unavailable")
)

// Limits shared by adapters and tools.
const (
	MaxTitleLen     = 200
	MaxBodyLen      = 20000
	MaxAttendees    = 50
	MaxMeetingMins  = 8 * 60
	MaxSearchLimit  = 20
	MaxPropertyKeys = 20
)

// MeetingInput describes a meeting to create.
type MeetingInput struct {
	Title       string
	Description string
	Start       time.Time
	Duration    time.Duration
	Timezone    string // IANA name, for display in invitations
	Attendees   []string
	// Notify asks the provider to email invitations (Google Calendar). Zoom does not email invitations through its API;
	// it records attendees as invitees and returns the join link.
	Notify bool
	// RequestID makes the create idempotent where the provider supports it (Google Calendar uses it to derive the event
	// id, so a retried request cannot create a second event).
	RequestID string
}

// Meeting is a scheduled meeting.
type Meeting struct {
	ID        string
	Provider  string // "google" or "zoom"
	Title     string
	Start     time.Time
	End       time.Time
	JoinURL   string // the link participants use (Google Meet or Zoom)
	Attendees []string
	// Created is false when a retried request returned the meeting an earlier call had already created.
	Created bool
}

// Interval is a busy period.
type Interval struct{ Start, End time.Time }

// Meetings creates, reads and cancels meetings.
type Meetings interface {
	Schedule(ctx context.Context, in MeetingInput) (Meeting, error)
	Get(ctx context.Context, id string) (Meeting, error)
	// Cancel removes the meeting; notify asks the provider to tell attendees. Cancelling an absent meeting succeeds.
	Cancel(ctx context.Context, id string, notify bool) error
}

// Calendar adds availability to Meetings.
type Calendar interface {
	Meetings
	FreeBusy(ctx context.Context, from, to time.Time) ([]Interval, error)
}

// NoteInput describes a page to create.
type NoteInput struct {
	Title string
	// Body is plain text; blank lines separate paragraphs.
	Body string
	// Properties are extra text properties of the page (a database row's columns), keyed by property name.
	Properties map[string]string
	RequestID  string
}

// Note is a page in the notes workspace.
type Note struct {
	ID    string
	Title string
	URL   string
}

// Notes reads and writes pages.
type Notes interface {
	CreatePage(ctx context.Context, in NoteInput) (Note, error)
	Append(ctx context.Context, pageID, text string) error
	Search(ctx context.Context, query string, limit int) ([]Note, error)
}

// Provider hands out the services a business has connected. A nil service means that provider is not connected.
type Provider interface {
	For(ctx context.Context, tenant string) (Services, error)
}

// Services are one business's connected providers.
type Services struct {
	Calendar Calendar // Google Calendar (with Google Meet links)
	Zoom     Meetings
	Notes    Notes
}
