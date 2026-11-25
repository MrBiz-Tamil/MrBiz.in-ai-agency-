// Package auth authenticates API callers with OIDC access tokens (Keycloak) and exposes the caller as a Principal.
//
// Authentication answers "who is this?" only. What the caller may do in a given business is decided by the
// tenant_members table (see internal/controlplane): a token never carries tenant authority by itself.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"
)

// Principal is an authenticated caller.
type Principal struct {
	// Sub is the stable OIDC subject.
	Sub string
}

// Verifier checks a bearer token.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Principal, error)
}

// OIDCVerifier verifies Keycloak-issued JWT access tokens: signature (keys fetched from the issuer's JWKS and
// rotated automatically), issuer, expiry and audience.
type OIDCVerifier struct{ v *oidc.IDTokenVerifier }

// NewOIDCVerifier performs OIDC discovery against issuer and returns a verifier requiring audience in `aud`.
func NewOIDCVerifier(ctx context.Context, issuer, audience string) (*OIDCVerifier, error) {
	if issuer == "" || audience == "" {
		return nil, errors.New("auth: issuer and audience are required")
	}
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc discovery for %s: %w", issuer, err)
	}
	return &OIDCVerifier{v: p.Verifier(&oidc.Config{
		ClientID:             audience,
		SupportedSigningAlgs: []string{"RS256", "ES256"},
	})}, nil
}

// NewOIDCVerifierWithRetry retries discovery until it succeeds or ctx ends, so the service can start while
// Keycloak is still booting.
func NewOIDCVerifierWithRetry(ctx context.Context, issuer, audience string, every time.Duration) (*OIDCVerifier, error) {
	for {
		v, err := NewOIDCVerifier(ctx, issuer, audience)
		if err == nil {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(every):
		}
	}
}

// Verify implements Verifier.
func (o *OIDCVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	tok, err := o.v.Verify(ctx, raw)
	if err != nil {
		return Principal{}, err
	}
	if tok.Subject == "" {
		return Principal{}, errors.New("auth: token has no subject")
	}
	return Principal{Sub: tok.Subject}, nil
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the authenticated caller.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Interceptor authenticates every unary call from its `Authorization: Bearer` header. Failures reveal nothing
// about why a token was rejected.
func Interceptor(v Verifier) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			h := req.Header().Get("Authorization")
			token, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || strings.TrimSpace(token) == "" {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
			}
			p, err := v.Verify(ctx, strings.TrimSpace(token))
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
			}
			return next(WithPrincipal(ctx, p), req)
		}
	})
}

// StaticVerifier accepts a fixed token->subject table. For tests and local development only.
type StaticVerifier map[string]string

func (s StaticVerifier) Verify(_ context.Context, raw string) (Principal, error) {
	if sub, ok := s[raw]; ok {
		return Principal{Sub: sub}, nil
	}
	return Principal{}, errors.New("unknown token")
}
