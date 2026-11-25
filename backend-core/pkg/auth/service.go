package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
)

const (
	SessionTTL       = 12 * time.Hour
	wsTicketTTL      = 60 * time.Second
	bcryptCost       = 12
	maxFailures      = 5
	maxFailuresPerIP = 50
	failureWindow    = 5 * time.Minute
	minPasswordLen   = 12
)

// Store is the persistence the auth service needs (satisfied by *repository.Repository).
type Store interface {
	FindUsersByEmail(ctx context.Context, email string) ([]repository.User, error)
	CreateSession(ctx context.Context, tokenHash []byte, userID, workspaceID string, expires time.Time) error
	GetSessionUser(ctx context.Context, tokenHash []byte) (*repository.User, time.Time, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error
}

func HashPassword(password string) (string, error) {
	if len(password) < minPasswordLen {
		return "", errors.New("password must be at least 12 characters")
	}
	if len(password) > 72 { // bcrypt silently truncates beyond 72 bytes
		return "", errors.New("password must be at most 72 bytes")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(b), err
}

// dummyHash is compared against when the email is unknown so that response time does not
// reveal whether an account exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcryptCost)

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

type wsTicket struct {
	p       Principal
	expires time.Time
}

type Service struct {
	store Store
	now   func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time // login failures per key
	tickets  map[string]wsTicket    // single-use websocket tickets
}

func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now, failures: map[string][]time.Time{}, tickets: map[string]wsTicket{}}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Service) limited(key string, max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-failureWindow)
	kept := s.failures[key][:0]
	for _, t := range s.failures[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.failures[key] = kept
	return len(kept) >= max
}

func (s *Service) recordFailure(key string) {
	s.mu.Lock()
	s.failures[key] = append(s.failures[key], s.now())
	s.mu.Unlock()
}

func (s *Service) clearFailures(key string) {
	s.mu.Lock()
	delete(s.failures, key)
	s.mu.Unlock()
}

// Login verifies credentials and creates a session. The returned token is shown once; only
// its SHA-256 is stored. `remote` (e.g. client IP) is combined with the email to rate limit.
func (s *Service) Login(ctx context.Context, email, password, remote string) (token string, expires time.Time, p *Principal, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	// Two limiters: per account (stops password guessing; caps lock-out at the window) and per
	// source address (a looser cap against one host spraying many accounts). Neither trusts
	// client-supplied headers.
	emailKey, ipKey := "email|"+email, "ip|"+remote
	if s.limited(emailKey, maxFailures) || s.limited(ipKey, maxFailuresPerIP) {
		return "", time.Time{}, nil, ErrRateLimited
	}
	fail := func() {
		s.recordFailure(emailKey)
		s.recordFailure(ipKey)
	}
	if email == "" || password == "" || len(password) > 72 {
		fail()
		return "", time.Time{}, nil, ErrBadCredentials
	}

	users, err := s.store.FindUsersByEmail(ctx, email)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	var matched *repository.User
	if len(users) == 0 {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password)) // equalise timing
	}
	for i := range users {
		if bcrypt.CompareHashAndPassword([]byte(users[i].PasswordHash), []byte(password)) == nil && matched == nil {
			matched = &users[i]
		}
	}
	if matched == nil {
		fail()
		return "", time.Time{}, nil, ErrBadCredentials
	}
	s.clearFailures(emailKey)

	token, err = randomToken()
	if err != nil {
		return "", time.Time{}, nil, err
	}
	expires = s.now().Add(SessionTTL)
	if err := s.store.CreateSession(ctx, hashToken(token), matched.ID, matched.WorkspaceID, expires); err != nil {
		return "", time.Time{}, nil, err
	}
	return token, expires, principalOf(matched), nil
}

func principalOf(u *repository.User) *Principal {
	return &Principal{UserID: u.ID, WorkspaceID: u.WorkspaceID, Email: u.Email, DisplayName: u.DisplayName, Role: Role(u.Role)}
}

// Authenticate resolves a session token to its principal.
func (s *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if token == "" || len(token) > 256 {
		return nil, ErrUnauthenticated
	}
	u, _, err := s.store.GetSessionUser(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrUnauthenticated
		}
		return nil, err
	}
	return principalOf(u), nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.store.DeleteSession(ctx, hashToken(token))
}

// IssueWSTicket creates a 60-second, single-use ticket so a browser can open a websocket
// without ever holding the long-lived session token.
func (s *Service) IssueWSTicket(p *Principal) (string, error) {
	t, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.tickets { // opportunistic cleanup
		if now.After(v.expires) {
			delete(s.tickets, k)
		}
	}
	s.tickets[t] = wsTicket{p: *p, expires: now.Add(wsTicketTTL)}
	return t, nil
}

// RedeemWSTicket consumes a ticket. A ticket works exactly once.
func (s *Service) RedeemWSTicket(ticket string) (*Principal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.tickets {
		if subtle.ConstantTimeCompare([]byte(k), []byte(ticket)) == 1 {
			delete(s.tickets, k)
			if s.now().After(v.expires) {
				return nil, false
			}
			p := v.p
			return &p, true
		}
	}
	return nil, false
}
