package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	"github.com/mrbiz/venture-os/internal/auth"
)

// fakeIssuer is a minimal OIDC provider: discovery document + JWKS, signing RS256 tokens.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"jwks_uri":                              f.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) token(t *testing.T, mutate func(jwt.MapClaims)) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": f.srv.URL, "sub": "user-123", "aud": "venture-api",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	if mutate != nil {
		mutate(c)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOIDCVerifier(t *testing.T) {
	iss := newIssuer(t)
	ctx := context.Background()
	v, err := auth.NewOIDCVerifier(ctx, iss.srv.URL, "venture-api")
	if err != nil {
		t.Fatal(err)
	}

	p, err := v.Verify(ctx, iss.token(t, nil))
	if err != nil || p.Sub != "user-123" {
		t.Fatalf("valid token: %+v, %v", p, err)
	}

	bad := map[string]func(jwt.MapClaims){
		"expired":        func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"wrong audience": func(c jwt.MapClaims) { c["aud"] = "someone-else" },
		"wrong issuer":   func(c jwt.MapClaims) { c["iss"] = "https://evil.example" },
		"no subject":     func(c jwt.MapClaims) { delete(c, "sub") },
	}
	for name, mutate := range bad {
		if _, err := v.Verify(ctx, iss.token(t, mutate)); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}

	// A token signed by a different key must not verify.
	other := newIssuer(t)
	forged := other.token(t, func(c jwt.MapClaims) { c["iss"] = iss.srv.URL })
	if _, err := v.Verify(ctx, forged); err == nil {
		t.Error("token signed by an unknown key accepted")
	}
	if _, err := v.Verify(ctx, "not.a.jwt"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestNewOIDCVerifierRequiresConfigAndReachableIssuer(t *testing.T) {
	if _, err := auth.NewOIDCVerifier(context.Background(), "", "x"); err == nil {
		t.Error("empty issuer accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := auth.NewOIDCVerifierWithRetry(ctx, "http://127.0.0.1:1", "x", 50*time.Millisecond); err == nil {
		t.Error("unreachable issuer should fail once ctx ends")
	}
}

func TestInterceptor(t *testing.T) {
	v := auth.StaticVerifier{"good": "user-1"}
	var seen string
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		p, _ := auth.FromContext(ctx)
		seen = p.Sub
		return nil, nil
	})
	handler := auth.Interceptor(v).WrapUnary(next)

	call := func(header string) error {
		req := connect.NewRequest(&struct{}{})
		if header != "" {
			req.Header().Set("Authorization", header)
		}
		_, err := handler(context.Background(), req)
		return err
	}
	if err := call("Bearer good"); err != nil || seen != "user-1" {
		t.Fatalf("good token: %v seen=%q", err, seen)
	}
	for _, h := range []string{"", "Bearer ", "Bearer nope", "Basic good", "good"} {
		err := call(h)
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("header %q: code = %v", h, connect.CodeOf(err))
		}
	}
}
