// Package webhook verifies that an inbound webhook really came from the provider, using constant-time comparison.
//
// A webhook endpoint is a public URL that causes things to happen; without verification anyone who finds it can forge
// events. Each provider signs differently, so this package has one small function per scheme, and callers pass the
// RAW request body (verifying a re-serialised body fails, correctly, because the bytes differ).
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid is returned for every verification failure, with no detail that would help a forger.
var ErrInvalid = errors.New("webhook: invalid signature")

func mac(secret string, parts ...string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	for _, p := range parts {
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

func eq(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// Hex verifies a hex-encoded HMAC-SHA256 of the body (for example Stripe-style and Notion's "sha256=<hex>" once the
// prefix is passed). prefix, if non-empty, must start the header value.
func Hex(secret string, body []byte, header, prefix string) error {
	if secret == "" {
		return ErrInvalid
	}
	h := strings.TrimSpace(header)
	if prefix != "" {
		var ok bool
		if h, ok = strings.CutPrefix(h, prefix); !ok {
			return ErrInvalid
		}
	}
	got, err := hex.DecodeString(h)
	if err != nil || !eq(got, mac(secret, string(body))) {
		return ErrInvalid
	}
	return nil
}

// Base64 verifies a base64-encoded HMAC-SHA256 of the body (Shopify's X-Shopify-Hmac-Sha256).
func Base64(secret string, body []byte, header string) error {
	if secret == "" {
		return ErrInvalid
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil || !eq(got, mac(secret, string(body))) {
		return ErrInvalid
	}
	return nil
}

// Token verifies a shared-secret token header (Google Calendar push notifications echo the token you registered in
// X-Goog-Channel-Token).
func Token(expected, got string) error {
	if expected == "" || !eq([]byte(expected), []byte(got)) {
		return ErrInvalid
	}
	return nil
}

// Zoom verifies Zoom's scheme: signature = "v0=" + hex(HMAC-SHA256(secret, "v0:" + timestamp + ":" + body)), with the
// timestamp in x-zm-request-timestamp. A timestamp further than tolerance from now is rejected, so a captured
// request cannot be replayed later.
func Zoom(secret string, body []byte, timestamp, signature string, now time.Time, tolerance time.Duration) error {
	if secret == "" {
		return ErrInvalid
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return ErrInvalid
	}
	age := now.Sub(time.Unix(sec, 0))
	if age < 0 {
		age = -age
	}
	if age > tolerance {
		return ErrInvalid
	}
	want := "v0=" + hex.EncodeToString(mac(secret, "v0:", timestamp, ":", string(body)))
	if !eq([]byte(want), []byte(strings.TrimSpace(signature))) {
		return ErrInvalid
	}
	return nil
}

// ZoomChallenge answers Zoom's endpoint.url_validation event: plainToken is echoed back with
// hex(HMAC-SHA256(secret, plainToken)). Without a correct answer Zoom will not enable the endpoint.
func ZoomChallenge(secret, plainToken string) (plain, encrypted string) {
	return plainToken, hex.EncodeToString(mac(secret, plainToken))
}

// Sign produces the Hex-scheme signature for a body; tests and fakes use it to play the provider.
func Sign(secret string, body []byte) string { return hex.EncodeToString(mac(secret, string(body))) }

// SignZoom produces Zoom's signature header value; tests and fakes use it to play Zoom.
func SignZoom(secret string, body []byte, timestamp string) string {
	return "v0=" + hex.EncodeToString(mac(secret, "v0:", timestamp, ":", string(body)))
}
