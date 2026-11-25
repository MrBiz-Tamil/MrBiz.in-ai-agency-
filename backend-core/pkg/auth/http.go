package auth

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// BearerToken extracts the token from an "Authorization: Bearer <token>" header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Middleware authenticates the bearer token and attaches the principal to the context.
// Requests without a valid session are rejected with 401, except websocket upgrades,
// which authenticate in the graphql-ws connection_init payload instead.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		p, err := s.Authenticate(r.Context(), BearerToken(r))
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Handlers registers /auth/login, /auth/logout, /auth/session and /auth/ws-ticket on mux.
func (s *Service) Handlers(mux *http.ServeMux) {
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/logout", s.handleLogout)
	mux.HandleFunc("/auth/session", s.handleSession)
	mux.HandleFunc("/auth/ws-ticket", s.handleWSTicket)
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	remote := clientIP(r)
	token, expires, p, err := s.Login(r.Context(), in.Email, in.Password, remote)
	switch {
	case errors.Is(err, ErrRateLimited):
		w.Header().Set("Retry-After", "300")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrBadCredentials):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"token": token, "expiresAt": expires.UTC().Format(time.RFC3339), "user": p})
	}
}

func (s *Service) principalFromRequest(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	p, err := s.Authenticate(r.Context(), BearerToken(r))
	if err != nil {
		code := http.StatusUnauthorized
		if !errors.Is(err, ErrUnauthenticated) {
			code = http.StatusInternalServerError
		}
		writeJSON(w, code, map[string]string{"error": "authentication required"})
		return nil, false
	}
	return p, true
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	_ = s.Logout(r.Context(), BearerToken(r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleSession(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.principalFromRequest(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"user": p})
	}
}

func (s *Service) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	p, ok := s.principalFromRequest(w, r)
	if !ok {
		return
	}
	t, err := s.IssueWSTicket(p)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ticket": t})
}
