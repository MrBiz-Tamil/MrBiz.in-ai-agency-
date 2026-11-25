// Package mcptoken mints and verifies the bearer tokens agents present to the MCP bus.
//
// A token binds an agent to exactly one tenant. The bus derives tenant_id from the verified token and never from
// a tool argument, so an agent (or a prompt-injected agent) cannot name another tenant. Tokens are HS256 JWTs
// signed with MCP_TOKEN_SIGNING_KEY, shared by the control plane (mints) and the bus (verifies).
package mcptoken

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	issuer   = "venture-os/controlplane"
	audience = "venture-os/mcpbus"
	// MinKeyBytes is the shortest signing key accepted (256 bits).
	MinKeyBytes = 32
)

// Claims identify who is calling the bus.
type Claims struct {
	TenantID uuid.UUID
	// Agent is the agent key the token was minted for; empty for a profile-wide token.
	Agent string
}

type claims struct {
	Tenant string `json:"tid"`
	Agent  string `json:"agent,omitempty"`
	jwt.RegisteredClaims
}

// Signer mints and verifies tokens.
type Signer struct {
	key []byte
	now func() time.Time
}

// NewSigner returns a Signer for key. Keys shorter than MinKeyBytes are rejected.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < MinKeyBytes {
		return nil, fmt.Errorf("mcptoken: signing key must be at least %d bytes, got %d", MinKeyBytes, len(key))
	}
	return &Signer{key: append([]byte(nil), key...), now: time.Now}, nil
}

// WithClock returns a copy using now as its clock (tests).
func (s *Signer) WithClock(now func() time.Time) *Signer {
	c := *s
	c.now = now
	return &c
}

// Mint issues a token for tenant (and optionally one agent) valid for ttl.
func (s *Signer) Mint(tenant uuid.UUID, agent string, ttl time.Duration) (string, error) {
	if tenant == uuid.Nil {
		return "", errors.New("mcptoken: tenant id is required")
	}
	if ttl <= 0 {
		return "", errors.New("mcptoken: ttl must be positive")
	}
	now := s.now()
	c := claims{
		Tenant: tenant.String(),
		Agent:  agent,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        uuid.NewString(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.key)
}

// Verify checks signature, algorithm, issuer, audience and expiry, and returns the bound claims.
func (s *Signer) Verify(token string) (Claims, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return s.key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil {
		return Claims{}, fmt.Errorf("mcptoken: %w", err)
	}
	tid, err := uuid.Parse(c.Tenant)
	if err != nil || tid == uuid.Nil {
		return Claims{}, errors.New("mcptoken: token has no valid tenant")
	}
	return Claims{TenantID: tid, Agent: c.Agent}, nil
}
