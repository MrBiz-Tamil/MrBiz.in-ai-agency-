package collab

import (
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func clean(field, v string, max int, required bool) (string, error) {
	v = strings.TrimSpace(v)
	if required && v == "" {
		return "", invalid("%s is required", field)
	}
	if utf8.RuneCountInString(v) > max {
		return "", invalid("%s is longer than %d characters", field, max)
	}
	if strings.ContainsRune(v, 0) {
		return "", invalid("%s contains a NUL character", field)
	}
	return v, nil
}

// NormalizeMeeting validates and canonicalises a meeting input: trimmed text, lower-cased de-duplicated attendee
// addresses, a real IANA time zone, a start in UTC and a sensible duration. Every adapter calls it first.
func NormalizeMeeting(in MeetingInput, now time.Time) (MeetingInput, error) {
	var err error
	if in.Title, err = clean("title", in.Title, MaxTitleLen, true); err != nil {
		return in, err
	}
	if in.Description, err = clean("description", in.Description, MaxBodyLen, false); err != nil {
		return in, err
	}
	if in.Start.IsZero() {
		return in, invalid("a start time is required")
	}
	if in.Start.Before(now.Add(-5 * time.Minute)) {
		return in, invalid("the start time is in the past")
	}
	if in.Duration < 5*time.Minute || in.Duration > MaxMeetingMins*time.Minute {
		return in, invalid("duration must be between 5 minutes and %d hours", MaxMeetingMins/60)
	}
	in.Start = in.Start.UTC()
	if in.Timezone = strings.TrimSpace(in.Timezone); in.Timezone == "" {
		in.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return in, invalid("timezone %q is not an IANA time zone name such as Asia/Kolkata", in.Timezone)
	}
	if len(in.Attendees) > MaxAttendees {
		return in, invalid("at most %d attendees", MaxAttendees)
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range in.Attendees {
		addr, err := mail.ParseAddress(strings.TrimSpace(a))
		if err != nil || addr.Name != "" && !strings.Contains(a, "<") {
			return in, invalid("attendee %q is not an email address", a)
		}
		e := strings.ToLower(addr.Address)
		if !strings.Contains(e, ".") || strings.ContainsAny(e, " \t\r\n,;") {
			return in, invalid("attendee %q is not an email address", a)
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	in.Attendees = out
	if len(in.RequestID) > 200 {
		return in, invalid("request id is too long")
	}
	return in, nil
}

// NormalizeNote validates a note input.
func NormalizeNote(in NoteInput) (NoteInput, error) {
	var err error
	if in.Title, err = clean("title", in.Title, MaxTitleLen, true); err != nil {
		return in, err
	}
	if in.Body, err = clean("body", in.Body, MaxBodyLen, false); err != nil {
		return in, err
	}
	if len(in.Properties) > MaxPropertyKeys {
		return in, invalid("at most %d properties", MaxPropertyKeys)
	}
	props := make(map[string]string, len(in.Properties))
	for k, v := range in.Properties {
		if k, err = clean("property name", k, 100, true); err != nil {
			return in, err
		}
		if props[k], err = clean("property "+k, v, 2000, false); err != nil {
			return in, err
		}
	}
	in.Properties = props
	return in, nil
}

// Paragraphs splits plain text into paragraphs (blank-line separated), each no longer than max characters, breaking
// long ones at spaces where possible. Notion limits a text block to 2000 characters.
func Paragraphs(text string, max int) []string {
	var out []string
	for _, p := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		p = strings.TrimSpace(p)
		for p != "" {
			r := []rune(p)
			if len(r) <= max {
				out = append(out, p)
				break
			}
			cut := max
			if i := strings.LastIndex(string(r[:max]), " "); i > max/2 {
				cut = utf8.RuneCountInString(string(r[:max])[:i])
			}
			out = append(out, strings.TrimSpace(string(r[:cut])))
			p = strings.TrimSpace(string(r[cut:]))
		}
	}
	return out
}
