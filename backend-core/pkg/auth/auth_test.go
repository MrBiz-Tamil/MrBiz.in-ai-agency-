package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
)

type memStore struct {
	mu       sync.Mutex
	users    []repository.User
	sessions map[string]repository.User
	expiry   map[string]time.Time
}

func newMemStore(t *testing.T, users ...repository.User) *memStore {
	return &memStore{users: users, sessions: map[string]repository.User{}, expiry: map[string]time.Time{}}
}

func (m *memStore) FindUsersByEmail(_ context.Context, email string) ([]repository.User, error) {
	var out []repository.User
	for _, u := range m.users {
		if u.Email == email {
			out = append(out, u)
		}
	}
	return out, nil
}
func (m *memStore) CreateSession(_ context.Context, h []byte, userID, ws string, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.ID == userID {
			m.sessions[string(h)] = u
			m.expiry[string(h)] = exp
		}
	}
	return nil
}
func (m *memStore) GetSessionUser(_ context.Context, h []byte) (*repository.User, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.sessions[string(h)]
	if !ok || time.Now().After(m.expiry[string(h)]) {
		return nil, time.Time{}, repository.ErrNotFound
	}
	return &u, m.expiry[string(h)], nil
}
func (m *memStore) DeleteSession(_ context.Context, h []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, string(h))
	return nil
}

const pw = "correct horse battery"

func service(t *testing.T) *Service {
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(newMemStore(t, repository.User{ID: "u1", WorkspaceID: "w1", Email: "ops@example.com", Role: "operator", PasswordHash: hash}))
}

func TestRolesAndPermissions(t *testing.T) {
	cases := map[Role]map[Permission]bool{
		RoleOwner:    {PermRead: true, PermDecide: true, PermOperate: true, PermAdmin: true},
		RoleAdmin:    {PermRead: true, PermDecide: true, PermOperate: true, PermAdmin: true},
		RoleOperator: {PermRead: true, PermDecide: true, PermOperate: true, PermAdmin: false},
		RoleAuditor:  {PermRead: true, PermDecide: false, PermOperate: false, PermAdmin: false},
		Role("root"): {PermRead: false, PermDecide: false},
	}
	for role, perms := range cases {
		for perm, want := range perms {
			if role.Can(perm) != want {
				t.Errorf("%s.Can(%s) = %v, want %v", role, perm, !want, want)
			}
		}
	}
	ctx := WithPrincipal(context.Background(), &Principal{Role: RoleAuditor})
	if _, err := Require(ctx, PermDecide); err != ErrForbidden {
		t.Errorf("auditor must not decide: %v", err)
	}
	if _, err := Require(context.Background(), PermRead); err != ErrUnauthenticated {
		t.Errorf("no principal must be unauthenticated: %v", err)
	}
}

func TestPasswordPolicy(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("short passwords must be rejected")
	}
	if _, err := HashPassword(strings.Repeat("a", 73)); err == nil {
		t.Error("over-72-byte passwords must be rejected (bcrypt truncation)")
	}
}

func TestLoginSessionLogout(t *testing.T) {
	s := service(t)
	ctx := context.Background()

	token, exp, p, err := s.Login(ctx, " OPS@example.com ", pw, "1.2.3.4")
	if err != nil || token == "" || p.Role != RoleOperator || p.WorkspaceID != "w1" {
		t.Fatalf("login: %v %+v %v", token, p, err)
	}
	if time.Until(exp) < 11*time.Hour {
		t.Errorf("session should last ~12h, expires %v", exp)
	}
	got, err := s.Authenticate(ctx, token)
	if err != nil || got.UserID != "u1" {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	if _, err := s.Authenticate(ctx, token+"x"); err != ErrUnauthenticated {
		t.Errorf("tampered token must fail: %v", err)
	}
	if _, err := s.Authenticate(ctx, ""); err != ErrUnauthenticated {
		t.Errorf("empty token must fail: %v", err)
	}
	if err := s.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); err != ErrUnauthenticated {
		t.Errorf("logged-out token must fail: %v", err)
	}
}

func TestLoginFailuresAreGenericAndRateLimited(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	if _, _, _, err := s.Login(ctx, "nobody@example.com", pw, "ip"); err != ErrBadCredentials {
		t.Errorf("unknown user: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, _, _, err := s.Login(ctx, "ops@example.com", "wrong-password-123", "ip"+string(rune('a'+i))); err != ErrBadCredentials {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Even the right password is refused while locked, from any address.
	if _, _, _, err := s.Login(ctx, "ops@example.com", pw, "other-ip"); err != ErrRateLimited {
		t.Errorf("account must be rate limited after 5 failures, got %v", err)
	}
	// The window expires.
	s.now = func() time.Time { return time.Now().Add(failureWindow + time.Second) }
	if _, _, _, err := s.Login(ctx, "ops@example.com", pw, "other-ip"); err != nil {
		t.Errorf("lockout must lift after the window: %v", err)
	}
}

func TestWSTicketIsSingleUseAndExpires(t *testing.T) {
	s := service(t)
	p := &Principal{UserID: "u1", WorkspaceID: "w1", Role: RoleOperator}
	tk, _ := s.IssueWSTicket(p)
	got, ok := s.RedeemWSTicket(tk)
	if !ok || got.UserID != "u1" {
		t.Fatalf("redeem: %+v %v", got, ok)
	}
	if _, ok := s.RedeemWSTicket(tk); ok {
		t.Error("ticket must be single use")
	}
	if _, ok := s.RedeemWSTicket("nonsense"); ok {
		t.Error("unknown ticket must fail")
	}
	tk, _ = s.IssueWSTicket(p)
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, ok := s.RedeemWSTicket(tk); ok {
		t.Error("expired ticket must fail")
	}
}

func TestHTTPFlow(t *testing.T) {
	s := service(t)
	mux := http.NewServeMux()
	s.Handlers(mux)
	mux.Handle("/protected", s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		_, _ = w.Write([]byte(p.Email))
	})))

	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("GET", "/protected", "", ""); rec.Code != 401 {
		t.Errorf("protected without token: %d", rec.Code)
	}
	if rec := do("GET", "/auth/login", "", ""); rec.Code != 405 {
		t.Errorf("login must be POST only: %d", rec.Code)
	}
	if rec := do("POST", "/auth/login", `{"email":"ops@example.com","password":"nope-nope-nope"}`, ""); rec.Code != 401 {
		t.Errorf("bad login: %d", rec.Code)
	}
	if rec := do("POST", "/auth/login", `not json`, ""); rec.Code != 400 {
		t.Errorf("malformed login: %d", rec.Code)
	}

	rec := do("POST", "/auth/login", `{"email":"ops@example.com","password":"`+pw+`"}`, "")
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("auth responses must not be cached")
	}

	if rec := do("GET", "/protected", "", out.Token); rec.Code != 200 || rec.Body.String() != "ops@example.com" {
		t.Errorf("protected with token: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/auth/session", "", out.Token); rec.Code != 200 {
		t.Errorf("session: %d", rec.Code)
	}
	if rec := do("POST", "/auth/ws-ticket", "", out.Token); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ticket") {
		t.Errorf("ws-ticket: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/auth/ws-ticket", "", ""); rec.Code != 401 {
		t.Errorf("ws-ticket without session: %d", rec.Code)
	}
	if rec := do("POST", "/auth/logout", "", out.Token); rec.Code != 200 {
		t.Errorf("logout: %d", rec.Code)
	}
	if rec := do("GET", "/auth/session", "", out.Token); rec.Code != 401 {
		t.Errorf("session after logout: %d", rec.Code)
	}
}
