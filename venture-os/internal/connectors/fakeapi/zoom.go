package fakeapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Zoom fakes Zoom's Server-to-Server OAuth token endpoint and the Meetings API.
type Zoom struct {
	base
	AccountID, ClientID, ClientSecret string
	TokenTTL                          time.Duration
	Now                               func() time.Time

	zmu      sync.Mutex
	meetings map[int64]map[string]any
	tokens   map[string]time.Time
	seq      int64
	tokenSeq int
}

// NewZoom starts the fake with the given credentials.
func NewZoom(accountID, clientID, clientSecret string) *Zoom {
	z := &Zoom{AccountID: accountID, ClientID: clientID, ClientSecret: clientSecret, TokenTTL: time.Hour, Now: time.Now,
		meetings: map[int64]map[string]any{}, tokens: map[string]time.Time{}, seq: 81234567000}
	z.srv = httptest.NewServer(http.HandlerFunc(z.serve))
	return z
}

// Meetings returns the number of live meetings.
func (z *Zoom) Meetings() int {
	z.zmu.Lock()
	defer z.zmu.Unlock()
	n := 0
	for id := range z.meetings {
		if id > 0 {
			n++
		}
	}
	return n
}

// Meeting returns a stored meeting's raw fields.
func (z *Zoom) Meeting(id int64) map[string]any {
	z.zmu.Lock()
	defer z.zmu.Unlock()
	return z.meetings[id]
}

// ExpireTokens invalidates every access token issued so far.
func (z *Zoom) ExpireTokens() { z.zmu.Lock(); z.tokens = map[string]time.Time{}; z.zmu.Unlock() }

func zoomErr(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, map[string]any{"code": code, "message": msg})
}

func (z *Zoom) serve(w http.ResponseWriter, r *http.Request) {
	c, injected := z.record(r)
	if injected != 0 {
		zoomErr(w, injected, 0, "injected failure")
		return
	}
	if r.URL.Path == "/oauth/token" {
		z.token(w, r, c)
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	z.zmu.Lock()
	exp, known := z.tokens[tok]
	valid := ok && known && z.Now().Before(exp)
	z.zmu.Unlock()
	if !valid {
		zoomErr(w, 401, 124, "Invalid access token.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "users/") && strings.HasSuffix(path, "/meetings"):
		z.create(w, c)
	case strings.HasPrefix(path, "meetings/"):
		z.item(w, r, c, strings.TrimPrefix(path, "meetings/"))
	default:
		zoomErr(w, 404, 3001, "Not found")
	}
}

func (z *Zoom) token(w http.ResponseWriter, r *http.Request, c Call) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(z.ClientID+":"+z.ClientSecret))
	switch {
	case r.Method != http.MethodPost || c.Query.Get("grant_type") != "account_credentials":
		writeJSON(w, 400, map[string]string{"reason": "Invalid grant_type", "error": "invalid_request"})
	case r.Header.Get("Authorization") != want:
		writeJSON(w, 401, map[string]string{"reason": "Invalid client_id or client_secret", "error": "invalid_client"})
	case c.Query.Get("account_id") != z.AccountID:
		writeJSON(w, 400, map[string]string{"reason": "Invalid account_id", "error": "invalid_request"})
	default:
		z.zmu.Lock()
		z.tokenSeq++
		tok := fmt.Sprintf("zoom-fake-%d", z.tokenSeq)
		z.tokens[tok] = z.Now().Add(z.TokenTTL)
		z.zmu.Unlock()
		writeJSON(w, 200, map[string]any{"access_token": tok, "token_type": "bearer", "expires_in": int(z.TokenTTL.Seconds()), "scope": "meeting:write:admin"})
	}
}

func (z *Zoom) create(w http.ResponseWriter, c Call) {
	var m map[string]any
	if json.Unmarshal(c.Body, &m) != nil {
		zoomErr(w, 400, 300, "Request Body should be a valid JSON object.")
		return
	}
	if t, _ := m["topic"].(string); strings.TrimSpace(t) == "" || len(t) > 200 {
		zoomErr(w, 400, 300, "Invalid field: topic")
		return
	}
	if typ, _ := m["type"].(float64); typ != 2 {
		zoomErr(w, 400, 300, "Only scheduled meetings (type 2) are supported by this fake")
		return
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(m["start_time"])); err != nil {
		zoomErr(w, 400, 300, "Invalid field: start_time")
		return
	}
	if d, _ := m["duration"].(float64); d < 1 || d > 1440 {
		zoomErr(w, 400, 300, "Invalid field: duration")
		return
	}
	z.zmu.Lock()
	z.seq++
	id := z.seq
	m["id"] = id
	m["join_url"] = fmt.Sprintf("https://zoom.us/j/%d?pwd=fakepwd", id)
	m["host_email"] = "host@example.com"
	z.meetings[id] = m
	z.zmu.Unlock()
	writeJSON(w, 201, m)
}

func (z *Zoom) item(w http.ResponseWriter, r *http.Request, c Call, rawID string) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		zoomErr(w, 400, 300, "Invalid meeting id")
		return
	}
	z.zmu.Lock()
	defer z.zmu.Unlock()
	m, ok := z.meetings[id]
	if !ok {
		zoomErr(w, 404, 3001, fmt.Sprintf("Meeting %d is not found or has expired.", id))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, m)
	case http.MethodDelete:
		m["_cancelReminder"] = c.Query.Get("cancel_meeting_reminder")
		delete(z.meetings, id)
		z.meetings[-id] = m // keep for inspection under a negative key
		w.WriteHeader(204)
	default:
		zoomErr(w, 405, 0, "Method not allowed")
	}
}

// CancelReminder reports the cancel_meeting_reminder value used when a meeting was deleted.
func (z *Zoom) CancelReminder(id int64) string {
	z.zmu.Lock()
	defer z.zmu.Unlock()
	s, _ := z.meetings[-id]["_cancelReminder"].(string)
	return s
}
