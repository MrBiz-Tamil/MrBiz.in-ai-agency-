package mcptoken_test

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/mcptoken"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func signer(t *testing.T) *mcptoken.Signer {
	t.Helper()
	s, err := mcptoken.NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTripBindsTenantAndAgent(t *testing.T) {
	s := signer(t)
	tenant := uuid.New()
	tok, err := s.Mint(tenant, "sales", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.TenantID != tenant || c.Agent != "sales" {
		t.Fatalf("claims = %+v", c)
	}
}

func TestRejectsWeakKeyAndBadMint(t *testing.T) {
	if _, err := mcptoken.NewSigner([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	s := signer(t)
	if _, err := s.Mint(uuid.Nil, "", time.Hour); err == nil {
		t.Fatal("nil tenant accepted")
	}
	if _, err := s.Mint(uuid.New(), "", 0); err == nil {
		t.Fatal("zero ttl accepted")
	}
}

func TestExpiry(t *testing.T) {
	now := time.Now()
	s := signer(t).WithClock(func() time.Time { return now })
	tok, _ := s.Mint(uuid.New(), "", time.Minute)
	later := signer(t).WithClock(func() time.Time { return now.Add(2 * time.Minute) })
	if _, err := later.Verify(tok); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestRejectsTamperingWrongKeyAndWrongAlgorithm(t *testing.T) {
	s := signer(t)
	tok, _ := s.Mint(uuid.New(), "", time.Hour)

	// Flip a character inside the payload segment.
	parts := strings.Split(tok, ".")
	parts[1] = parts[1][:len(parts[1])-2] + "AA"
	if _, err := s.Verify(strings.Join(parts, ".")); err == nil {
		t.Fatal("tampered token accepted")
	}

	other, _ := mcptoken.NewSigner([]byte("ffffffffffffffffffffffffffffffff"))
	if _, err := other.Verify(tok); err == nil {
		t.Fatal("token verified under a different key")
	}

	// alg=none must never be honoured.
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"tid": uuid.NewString(), "iss": "venture-os/controlplane", "aud": "venture-os/mcpbus",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	raw, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := s.Verify(raw); err == nil {
		t.Fatal("unsigned token accepted")
	}

	// A correctly signed token for another audience (e.g. reused from elsewhere) is refused.
	wrongAud := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"tid": uuid.NewString(), "iss": "venture-os/controlplane", "aud": "someone-else",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	raw, _ = wrongAud.SignedString(key)
	if _, err := s.Verify(raw); err == nil {
		t.Fatal("token for another audience accepted")
	}
}

func TestTokensAreUniquePerMint(t *testing.T) {
	s := signer(t)
	tenant := uuid.New()
	a, _ := s.Mint(tenant, "", time.Hour)
	b, _ := s.Mint(tenant, "", time.Hour)
	if a == b {
		t.Fatal("two mints produced identical tokens (jti missing)")
	}
}
