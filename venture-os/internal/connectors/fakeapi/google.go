// Package fakeapi holds in-process fakes of the third-party APIs the connectors call. They are written from the
// providers' documented behaviour and are deliberately strict (required headers and parameters, size limits, id
// formats, token expiry), so an adapter that gets one wrong fails its tests here instead of in production.
//
// They prove an adapter is consistent with that understanding of the API. They do NOT prove it works against the real
// product: that needs the provider's sandbox and real credentials, which tests cannot have.
package fakeapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Call is one request the fake received.
type Call struct {
	Method, Path string
	Query        url.Values
	Header       http.Header
	Body         []byte
}

type base struct {
	mu    sync.Mutex
	calls []Call
	fail  []int // statuses to answer with, once each, before behaving normally
	srv   *httptest.Server
}

func (b *base) URL() string { return b.srv.URL }
func (b *base) Close()      { b.srv.Close() }

// Calls returns a copy of everything received so far.
func (b *base) Calls() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Call(nil), b.calls...)
}

// FailNext makes the next requests answer with these statuses (one each) before resuming normal behaviour.
func (b *base) FailNext(statuses ...int) {
	b.mu.Lock()
	b.fail = append(b.fail, statuses...)
	b.mu.Unlock()
}

func (b *base) record(r *http.Request) (Call, int) {
	body, _ := io.ReadAll(r.Body)
	c := Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, c)
	if len(b.fail) > 0 {
		s := b.fail[0]
		b.fail = b.fail[1:]
		return c, s
	}
	return c, 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func googleErr(w http.ResponseWriter, code int, reason, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": msg, "errors": []map[string]string{{"reason": reason}}}})
}

// ───────────────────────────── Google Calendar ─────────────────────────────

// Google fakes the OAuth token endpoint and the Calendar v3 events and free/busy endpoints.
type Google struct {
	base
	ClientID, ClientSecret, RefreshToken string
	// TokenTTL is how long an access token lives; default one hour.
	TokenTTL time.Duration
	// Now is the clock used for token expiry.
	Now func() time.Time
	// PendingMeet makes a create answer with the conference still pending (no link yet); a later read has it.
	PendingMeet bool
	// Busy is what free/busy reports.
	Busy [][2]time.Time

	events    map[string]map[string]any
	revoked   bool
	tokens    map[string]time.Time
	tokenSeq  int
	linkSeq   int
	pollsLeft int
}

var googleEventID = regexp.MustCompile(`^[a-v0-9]+$`)

// NewGoogle starts the fake with the given credentials.
func NewGoogle(clientID, clientSecret, refreshToken string) *Google {
	g := &Google{ClientID: clientID, ClientSecret: clientSecret, RefreshToken: refreshToken, TokenTTL: time.Hour, Now: time.Now,
		events: map[string]map[string]any{}, tokens: map[string]time.Time{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	return g
}

// Revoke makes the refresh token invalid, as when the user removes the app from their Google account.
func (g *Google) Revoke() { g.mu.Lock(); g.revoked = true; g.mu.Unlock() }

// ExpireTokens invalidates every access token issued so far, forcing a refresh.
func (g *Google) ExpireTokens() { g.mu.Lock(); g.tokens = map[string]time.Time{}; g.mu.Unlock() }

// Events returns the stored events.
func (g *Google) Events() map[string]map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]map[string]any{}
	for k, v := range g.events {
		out[k] = v
	}
	return out
}

func (g *Google) serve(w http.ResponseWriter, r *http.Request) {
	c, injected := g.record(r)
	if injected != 0 {
		googleErr(w, injected, "backendError", "injected failure")
		return
	}
	if r.URL.Path == "/token" {
		g.token(w, c)
		return
	}
	if !g.authorized(r) {
		googleErr(w, 401, "authError", "Invalid Credentials")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && path == "freeBusy":
		g.freeBusy(w, c)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/events") && strings.HasPrefix(path, "calendars/"):
		g.insert(w, c)
	case strings.Contains(path, "/events/"):
		g.item(w, r, c, path[strings.LastIndex(path, "/")+1:])
	default:
		googleErr(w, 404, "notFound", "Not Found")
	}
}

func (g *Google) authorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	defer g.mu.Unlock()
	exp, known := g.tokens[tok]
	return ok && known && g.Now().Before(exp)
}

func (g *Google) token(w http.ResponseWriter, c Call) {
	form, _ := url.ParseQuery(string(c.Body))
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case form.Get("grant_type") != "refresh_token":
		writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
	case form.Get("client_id") != g.ClientID || form.Get("client_secret") != g.ClientSecret:
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
	case g.revoked || form.Get("refresh_token") != g.RefreshToken:
		writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
	default:
		g.tokenSeq++
		tok := fmt.Sprintf("ya29.fake-%d", g.tokenSeq)
		g.tokens[tok] = g.Now().Add(g.TokenTTL)
		writeJSON(w, 200, map[string]any{"access_token": tok, "expires_in": int(g.TokenTTL.Seconds()), "token_type": "Bearer"})
	}
}

func (g *Google) insert(w http.ResponseWriter, c Call) {
	var ev map[string]any
	if err := json.Unmarshal(c.Body, &ev); err != nil {
		googleErr(w, 400, "parseError", "Parse Error")
		return
	}
	if su := c.Query.Get("sendUpdates"); su != "all" && su != "none" && su != "externalOnly" {
		googleErr(w, 400, "invalid", "Invalid sendUpdates")
		return
	}
	for _, k := range []string{"start", "end"} {
		m, _ := ev[k].(map[string]any)
		if _, err := time.Parse(time.RFC3339, fmt.Sprint(m["dateTime"])); err != nil {
			googleErr(w, 400, "invalid", "Invalid value for: "+k)
			return
		}
	}
	id, _ := ev["id"].(string)
	g.mu.Lock()
	defer g.mu.Unlock()
	if id == "" {
		g.linkSeq++
		id = fmt.Sprintf("gen%020d", g.linkSeq)
	} else if !googleEventID.MatchString(id) || len(id) < 5 || len(id) > 1024 {
		googleErr(w, 400, "invalid", "Invalid resource id value.")
		return
	} else if _, exists := g.events[id]; exists {
		googleErr(w, 409, "duplicate", "The requested identifier already exists.")
		return
	}
	ev["id"], ev["status"] = id, "confirmed"
	ev["_sendUpdates"] = c.Query.Get("sendUpdates")
	// Conferences are only created when the caller opts in with conferenceDataVersion=1.
	if cd, ok := ev["conferenceData"].(map[string]any); ok && c.Query.Get("conferenceDataVersion") == "1" {
		if cr, ok := cd["createRequest"].(map[string]any); ok {
			if key, _ := cr["conferenceSolutionKey"].(map[string]any); key["type"] == "hangoutsMeet" {
				g.linkSeq++
				link := fmt.Sprintf("https://meet.google.com/fak-e%04d-xyz", g.linkSeq)
				ev["_link"] = link
				if g.PendingMeet {
					g.pollsLeft = 1
					cr["status"] = map[string]string{"statusCode": "pending"}
				} else {
					cr["status"] = map[string]string{"statusCode": "success"}
					ev["hangoutLink"] = link
					cd["entryPoints"] = []map[string]string{{"entryPointType": "video", "uri": link}}
				}
			}
		}
	} else {
		delete(ev, "conferenceData")
	}
	g.events[id] = ev
	writeJSON(w, 200, publicEvent(ev))
}

func publicEvent(ev map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range ev {
		if !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out
}

func (g *Google) item(w http.ResponseWriter, r *http.Request, c Call, id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ev, ok := g.events[id]
	switch r.Method {
	case http.MethodGet:
		if !ok {
			googleErr(w, 404, "notFound", "Not Found")
			return
		}
		if g.pollsLeft > 0 { // the conference finished being created
			g.pollsLeft--
			if g.pollsLeft == 0 {
				ev["hangoutLink"] = ev["_link"]
				if cd, ok := ev["conferenceData"].(map[string]any); ok {
					cd["entryPoints"] = []map[string]string{{"entryPointType": "video", "uri": fmt.Sprint(ev["_link"])}}
					if cr, ok := cd["createRequest"].(map[string]any); ok {
						cr["status"] = map[string]string{"statusCode": "success"}
					}
				}
			}
		}
		writeJSON(w, 200, publicEvent(ev))
	case http.MethodDelete:
		if !ok {
			googleErr(w, 404, "notFound", "Not Found")
			return
		}
		if ev["status"] == "cancelled" {
			googleErr(w, 410, "deleted", "Resource has been deleted")
			return
		}
		ev["status"], ev["_cancelUpdates"] = "cancelled", c.Query.Get("sendUpdates")
		w.WriteHeader(204)
	default:
		googleErr(w, 405, "methodNotAllowed", "Method Not Allowed")
	}
}

// CancelNotice reports the sendUpdates value a delete used.
func (g *Google) CancelNotice(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, _ := g.events[id]["_cancelUpdates"].(string)
	return s
}

// SendUpdates reports the sendUpdates value a create used.
func (g *Google) SendUpdates(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, _ := g.events[id]["_sendUpdates"].(string)
	return s
}

func (g *Google) freeBusy(w http.ResponseWriter, c Call) {
	var req struct {
		TimeMin, TimeMax string
		Items            []struct{ ID string }
	}
	if json.Unmarshal(c.Body, &req) != nil || len(req.Items) == 0 {
		googleErr(w, 400, "invalid", "Invalid request")
		return
	}
	var busy []map[string]string
	for _, b := range g.Busy {
		busy = append(busy, map[string]string{"start": b[0].UTC().Format(time.RFC3339), "end": b[1].UTC().Format(time.RFC3339)})
	}
	cals := map[string]any{}
	for _, it := range req.Items {
		cals[it.ID] = map[string]any{"busy": busy}
	}
	writeJSON(w, 200, map[string]any{"timeMin": req.TimeMin, "timeMax": req.TimeMax, "calendars": cals})
}
