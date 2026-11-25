package collab_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
)

var now = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func good() collab.MeetingInput {
	return collab.MeetingInput{Title: " Buyer call ", Start: now.Add(24 * time.Hour), Duration: 30 * time.Minute, Timezone: "Asia/Kolkata", Attendees: []string{"Ada@Acme.COM", "ada@acme.com", "Bob <bob@example.org>"}}
}

func TestNormalizeMeetingCanonicalizes(t *testing.T) {
	in, err := collab.NormalizeMeeting(good(), now)
	if err != nil {
		t.Fatal(err)
	}
	if in.Title != "Buyer call" || len(in.Attendees) != 2 || in.Attendees[0] != "ada@acme.com" || in.Attendees[1] != "bob@example.org" {
		t.Fatalf("%+v", in)
	}
	if g := good(); func() bool { g.Timezone = ""; r, _ := collab.NormalizeMeeting(g, now); return r.Timezone != "UTC" }() {
		t.Fatal("an empty timezone must become UTC")
	}
}

func TestNormalizeMeetingRejectsBadInput(t *testing.T) {
	for name, mutate := range map[string]func(*collab.MeetingInput){
		"no title":           func(m *collab.MeetingInput) { m.Title = "  " },
		"long title":         func(m *collab.MeetingInput) { m.Title = strings.Repeat("x", 201) },
		"no start":           func(m *collab.MeetingInput) { m.Start = time.Time{} },
		"in the past":        func(m *collab.MeetingInput) { m.Start = now.Add(-time.Hour) },
		"too short":          func(m *collab.MeetingInput) { m.Duration = time.Minute },
		"too long":           func(m *collab.MeetingInput) { m.Duration = 9 * time.Hour },
		"bad timezone":       func(m *collab.MeetingInput) { m.Timezone = "Mars/Olympus" },
		"bad attendee":       func(m *collab.MeetingInput) { m.Attendees = []string{"not-an-email"} },
		"attendee no dot":    func(m *collab.MeetingInput) { m.Attendees = []string{"a@b"} },
		"header injection":   func(m *collab.MeetingInput) { m.Attendees = []string{"a@b.com\r\nBcc: x@y.com"} },
		"too many":           func(m *collab.MeetingInput) { m.Attendees = make([]string, 51) },
		"NUL in description": func(m *collab.MeetingInput) { m.Description = "a\x00b" },
	} {
		m := good()
		mutate(&m)
		if _, err := collab.NormalizeMeeting(m, now); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	m := good()
	m.Start = now.Add(-2 * time.Minute) // a start that is "now" is fine: clocks differ by seconds
	if _, err := collab.NormalizeMeeting(m, now); err != nil {
		t.Errorf("a start within the grace period: %v", err)
	}
}

func TestNormalizeNote(t *testing.T) {
	n, err := collab.NormalizeNote(collab.NoteInput{Title: " Call notes ", Body: "x", Properties: map[string]string{"Status": " Open "}})
	if err != nil || n.Title != "Call notes" || n.Properties["Status"] != "Open" {
		t.Fatalf("%+v %v", n, err)
	}
	for name, in := range map[string]collab.NoteInput{
		"no title":      {Title: ""},
		"long body":     {Title: "t", Body: strings.Repeat("x", 20001)},
		"empty prop":    {Title: "t", Properties: map[string]string{"": "v"}},
		"long property": {Title: "t", Properties: map[string]string{"k": strings.Repeat("x", 2001)}},
	} {
		if _, err := collab.NormalizeNote(in); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParagraphsSplitAtBlankLinesAndRespectTheLimit(t *testing.T) {
	got := collab.Paragraphs("one\n\ntwo words here\r\n\r\n   \n\nthree", 100)
	if strings.Join(got, "|") != "one|two words here|three" {
		t.Fatalf("%q", got)
	}
	long := strings.Repeat("word ", 50) // 250 chars
	parts := collab.Paragraphs(long, 100)
	if len(parts) < 3 {
		t.Fatalf("%d parts", len(parts))
	}
	for _, p := range parts {
		if len([]rune(p)) > 100 || strings.HasPrefix(p, " ") || strings.HasSuffix(p, " ") {
			t.Fatalf("bad part %q", p)
		}
	}
	if strings.Join(parts, " ") != strings.TrimSpace(long) {
		t.Fatal("splitting lost or changed text")
	}
	hindi := strings.Repeat("मसाला ", 100)
	for _, p := range collab.Paragraphs(hindi, 50) {
		if len([]rune(p)) > 50 {
			t.Fatalf("limit counted in bytes, not characters: %d", len([]rune(p)))
		}
	}
	if len(collab.Paragraphs("", 10)) != 0 {
		t.Fatal("empty text has no paragraphs")
	}
}
