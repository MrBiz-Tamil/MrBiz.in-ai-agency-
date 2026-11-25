package webhook_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/webhook"
)

var body = []byte(`{"event":"meeting.started","payload":{"id":"123"}}`)

func TestHexAndPrefixedHex(t *testing.T) {
	sig := webhook.Sign("s3cret", body)
	if err := webhook.Hex("s3cret", body, sig, ""); err != nil {
		t.Fatal(err)
	}
	if err := webhook.Hex("s3cret", body, "sha256="+sig, "sha256="); err != nil {
		t.Fatalf("prefixed: %v", err)
	}
	for name, c := range map[string]struct {
		secret string
		body   []byte
		header string
		prefix string
	}{
		"wrong secret":          {"other", body, sig, ""},
		"tampered body":         {"s3cret", append([]byte(nil), append(body, ' ')...), sig, ""},
		"empty secret":          {"", body, sig, ""},
		"empty header":          {"s3cret", body, "", ""},
		"not hex":               {"s3cret", body, "zz" + sig[2:], ""},
		"missing required pref": {"s3cret", body, sig, "sha256="},
		"truncated":             {"s3cret", body, sig[:len(sig)-2], ""},
	} {
		if err := webhook.Hex(c.secret, c.body, c.header, c.prefix); !errors.Is(err, webhook.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestBase64MatchesShopifyStyle(t *testing.T) {
	m := hmac.New(sha256.New, []byte("shpss_x"))
	m.Write(body)
	sig := base64.StdEncoding.EncodeToString(m.Sum(nil))
	if err := webhook.Base64("shpss_x", body, sig); err != nil {
		t.Fatal(err)
	}
	if err := webhook.Base64("shpss_x", body, "not base64!"); err == nil {
		t.Fatal("garbage accepted")
	}
	if err := webhook.Base64("shpss_x", []byte("other"), sig); err == nil {
		t.Fatal("wrong body accepted")
	}
}

func TestTokenIsExactAndNeverEmpty(t *testing.T) {
	if err := webhook.Token("abc", "abc"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"abc", "abd"}, {"abc", ""}, {"", ""}, {"abc", "abcd"}} {
		if err := webhook.Token(c[0], c[1]); err == nil {
			t.Errorf("token %q vs %q accepted", c[0], c[1])
		}
	}
}

func TestZoomSignatureReplayWindowAndChallenge(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := webhook.SignZoom("zoom-secret", body, ts)
	if err := webhook.Zoom("zoom-secret", body, ts, sig, now.Add(2*time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("fresh request: %v", err)
	}
	if err := webhook.Zoom("zoom-secret", body, ts, sig, now.Add(6*time.Minute), 5*time.Minute); err == nil {
		t.Error("a replay outside the window was accepted")
	}
	if err := webhook.Zoom("zoom-secret", body, ts, sig, now.Add(-6*time.Minute), 5*time.Minute); err == nil {
		t.Error("a timestamp from the future was accepted")
	}
	if err := webhook.Zoom("zoom-secret", body, "abc", sig, now, 5*time.Minute); err == nil {
		t.Error("a non-numeric timestamp was accepted")
	}
	if err := webhook.Zoom("zoom-secret", append([]byte("x"), body...), ts, sig, now, 5*time.Minute); err == nil {
		t.Error("a tampered body was accepted")
	}
	// A valid signature for one timestamp must not validate another (the timestamp is signed).
	other := strconv.FormatInt(now.Unix()+1, 10)
	if err := webhook.Zoom("zoom-secret", body, other, sig, now, 5*time.Minute); err == nil {
		t.Error("a signature was valid for a different timestamp")
	}
	plain, enc := webhook.ZoomChallenge("zoom-secret", "q1w2e3")
	if plain != "q1w2e3" || len(enc) != 64 {
		t.Fatalf("challenge = %q %q", plain, enc)
	}
	if _, enc2 := webhook.ZoomChallenge("other", "q1w2e3"); enc2 == enc {
		t.Error("the challenge answer does not depend on the secret")
	}
}
