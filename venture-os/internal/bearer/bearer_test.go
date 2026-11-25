package bearer_test

import (
	"testing"

	"github.com/mrbiz/venture-os/internal/bearer"
)

func TestSchemeIsCaseInsensitiveAndOtherInputIsRefused(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abc.def":    "abc.def",
		"bearer abc.def":    "abc.def",
		"BEARER abc.def":    "abc.def",
		"bEaReR abc.def":    "abc.def",
		"Bearer   spaced  ": "spaced",
	} {
		if got, ok := bearer.Token(in); !ok || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "Bearer", "Bearer ", "Bearer    ", "Basic dXNlcjpwYXNz", "Token abc", "Bearerabc", "abc", " Bearer abc"} {
		if got, ok := bearer.Token(in); ok {
			t.Errorf("%q was accepted as %q", in, got)
		}
	}
}
