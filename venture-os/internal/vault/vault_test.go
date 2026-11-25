package vault_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/vault"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestSealAndOpenRoundTrip(t *testing.T) {
	v, err := vault.New(map[int][]byte{1: key(1)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	tn := uuid.New()
	s, err := v.Seal(tn, "google", []byte(`{"refresh_token":"1//abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(s.Ciphertext, []byte("refresh_token")) || s.KeyVersion != 1 || len(s.Nonce) != 12 {
		t.Fatalf("sealed = %+v", s)
	}
	got, err := v.Open(tn, "google", s)
	if err != nil || string(got) != `{"refresh_token":"1//abc"}` {
		t.Fatalf("open = %q %v", got, err)
	}
	s2, _ := v.Seal(tn, "google", []byte(`{"refresh_token":"1//abc"}`))
	if bytes.Equal(s.Nonce, s2.Nonce) || bytes.Equal(s.Ciphertext, s2.Ciphertext) {
		t.Fatal("the same secret sealed twice must differ (random nonce)")
	}
}

func TestACiphertextCannotBeMovedToAnotherTenantOrName(t *testing.T) {
	v, _ := vault.New(map[int][]byte{1: key(1)}, 1)
	a, b := uuid.New(), uuid.New()
	s, _ := v.Seal(a, "zoom", []byte("secret"))
	if _, err := v.Open(b, "zoom", s); !errors.Is(err, vault.ErrUnreadable) {
		t.Fatalf("another tenant opened it: %v", err)
	}
	if _, err := v.Open(a, "notion", s); !errors.Is(err, vault.ErrUnreadable) {
		t.Fatalf("another name opened it: %v", err)
	}
	if _, err := v.Open(a, "zoom", s); err != nil {
		t.Fatalf("the right owner must still open it: %v", err)
	}
}

func TestTamperingWrongKeyAndBadShapeAreUnreadable(t *testing.T) {
	v, _ := vault.New(map[int][]byte{1: key(1)}, 1)
	tn := uuid.New()
	s, _ := v.Seal(tn, "n", []byte("secret value"))
	flip := func(b []byte) []byte { c := bytes.Clone(b); c[0] ^= 1; return c }
	for name, bad := range map[string]vault.Sealed{
		"flipped ciphertext": {KeyVersion: 1, Nonce: s.Nonce, Ciphertext: flip(s.Ciphertext)},
		"flipped nonce":      {KeyVersion: 1, Nonce: flip(s.Nonce), Ciphertext: s.Ciphertext},
		"truncated":          {KeyVersion: 1, Nonce: s.Nonce, Ciphertext: s.Ciphertext[:len(s.Ciphertext)-1]},
		"unknown key":        {KeyVersion: 9, Nonce: s.Nonce, Ciphertext: s.Ciphertext},
		"short nonce":        {KeyVersion: 1, Nonce: s.Nonce[:4], Ciphertext: s.Ciphertext},
		"empty":              {},
	} {
		if _, err := v.Open(tn, "n", bad); !errors.Is(err, vault.ErrUnreadable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other, _ := vault.New(map[int][]byte{1: key(2)}, 1)
	if _, err := other.Open(tn, "n", s); !errors.Is(err, vault.ErrUnreadable) {
		t.Fatalf("a different master key opened it: %v", err)
	}
	if err := func() error { _, e := v.Open(tn, "n", flip2(s)); return e }(); err == nil || strings.Contains(err.Error(), "secret value") {
		t.Fatalf("errors must not reveal anything: %v", err)
	}
}

func flip2(s vault.Sealed) vault.Sealed {
	s.Ciphertext = append([]byte(nil), s.Ciphertext...)
	s.Ciphertext[1] ^= 0xff
	return s
}

func TestKeyRotation(t *testing.T) {
	old, _ := vault.New(map[int][]byte{1: key(1)}, 1)
	tn := uuid.New()
	s1, _ := old.Seal(tn, "n", []byte("v1 secret"))
	rotated, err := vault.New(map[int][]byte{1: key(1), 2: key(2)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open(tn, "n", s1); err != nil || string(got) != "v1 secret" || !rotated.NeedsRotation(s1) {
		t.Fatalf("an old secret must stay readable and be flagged: %q %v", got, err)
	}
	s2, _ := rotated.Seal(tn, "n", []byte("v2 secret"))
	if s2.KeyVersion != 2 || rotated.NeedsRotation(s2) {
		t.Fatalf("new secrets use the current key: %+v", s2)
	}
	retired, _ := vault.New(map[int][]byte{2: key(2)}, 2)
	if _, err := retired.Open(tn, "n", s1); !errors.Is(err, vault.ErrUnreadable) {
		t.Fatal("once key 1 is retired its secrets are unreadable until re-sealed")
	}
}

func TestInputsAreValidated(t *testing.T) {
	if _, err := vault.New(nil, 1); err == nil {
		t.Fatal("no keys")
	}
	if _, err := vault.New(map[int][]byte{1: []byte("short")}, 1); err == nil {
		t.Fatal("a short key")
	}
	if _, err := vault.New(map[int][]byte{1: key(1)}, 2); err == nil {
		t.Fatal("current key missing")
	}
	if _, err := vault.New(map[int][]byte{0: key(1)}, 0); err == nil {
		t.Fatal("version 0")
	}
	v, _ := vault.New(map[int][]byte{1: key(1)}, 1)
	for name, f := range map[string]func() error{
		"nil tenant":   func() error { _, e := v.Seal(uuid.Nil, "n", []byte("x")); return e },
		"empty name":   func() error { _, e := v.Seal(uuid.New(), "", []byte("x")); return e },
		"empty secret": func() error { _, e := v.Seal(uuid.New(), "n", nil); return e },
		"huge secret":  func() error { _, e := v.Seal(uuid.New(), "n", make([]byte, vault.MaxSecretBytes+1)); return e },
	} {
		if f() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFromEnv(t *testing.T) {
	k1, k2 := base64.StdEncoding.EncodeToString(key(1)), base64.StdEncoding.EncodeToString(key(2))
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	v, err := vault.FromEnv(env(map[string]string{"VENTURE_SECRETS_KEYS": "1:" + k1 + ", 2:" + k2}))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := v.Seal(uuid.New(), "n", []byte("x"))
	if s.KeyVersion != 2 {
		t.Fatalf("the highest version is current by default: %d", s.KeyVersion)
	}
	v, _ = vault.FromEnv(env(map[string]string{"VENTURE_SECRETS_KEYS": "1:" + k1 + ",2:" + k2, "VENTURE_SECRETS_CURRENT": "1"}))
	if s, _ := v.Seal(uuid.New(), "n", []byte("x")); s.KeyVersion != 1 {
		t.Fatal("VENTURE_SECRETS_CURRENT must win")
	}
	for name, m := range map[string]map[string]string{
		"unset":       {},
		"no version":  {"VENTURE_SECRETS_KEYS": k1},
		"bad base64":  {"VENTURE_SECRETS_KEYS": "1:@@@"},
		"short key":   {"VENTURE_SECRETS_KEYS": "1:" + base64.StdEncoding.EncodeToString([]byte("short"))},
		"bad current": {"VENTURE_SECRETS_KEYS": "1:" + k1, "VENTURE_SECRETS_CURRENT": "x"},
	} {
		if _, err := vault.FromEnv(env(m)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
