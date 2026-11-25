package vault

import (
	"strings"
	"testing"
)

func TestRoundTripAndWorkspaceBinding(t *testing.T) {
	key, _ := GenerateKey()
	v, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(`{"bearer_token":"s3cr3t"}`)
	ct, err := v.Encrypt("ws-a", secret)
	if err != nil || strings.Contains(ct, "s3cr3t") {
		t.Fatalf("encrypt: %q %v", ct, err)
	}
	pt, err := v.Decrypt("ws-a", ct)
	if err != nil || string(pt) != string(secret) {
		t.Fatalf("decrypt: %q %v", pt, err)
	}
	if _, err := v.Decrypt("ws-b", ct); err == nil {
		t.Error("ciphertext must not decrypt under a different workspace")
	}
	// Fresh nonce each time.
	ct2, _ := v.Encrypt("ws-a", secret)
	if ct == ct2 {
		t.Error("two encryptions of the same plaintext must differ")
	}
}

func TestRejectsTamperingAndBadKeys(t *testing.T) {
	v, _ := NewRandom()
	ct, _ := v.Encrypt("w", []byte("x"))
	tampered := ct[:len(ct)-2] + "AA"
	for name, payload := range map[string]string{"tampered": tampered, "no prefix": "abc", "garbage": "v1:!!!", "short": "v1:AAAA", "empty": ""} {
		if _, err := v.Decrypt("w", payload); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
	other, _ := NewRandom()
	if _, err := other.Decrypt("w", ct); err == nil {
		t.Error("a different key must not decrypt")
	}
	for _, bad := range []string{"", "not-base64!", "c2hvcnQ="} {
		if _, err := New(bad); err == nil {
			t.Errorf("key %q must be rejected", bad)
		}
	}
}
