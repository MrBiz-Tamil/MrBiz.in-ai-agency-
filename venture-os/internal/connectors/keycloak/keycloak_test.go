package keycloak_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/connectors/keycloak"
	"github.com/mrbiz/venture-os/internal/ports"
)

var ctx = context.Background()

// fakeKC reproduces the parts of the Keycloak Admin API the adapter uses, with the documented status codes
// (201 + Location on create, 409 on duplicates, 204 on delete, members body is a bare JSON string).
type fakeKC struct {
	mu         sync.Mutex
	orgs       map[string]map[string]any
	members    map[string]map[string]bool
	users      map[string]bool
	tokenCalls int
	revokeNext bool // answer the next admin call with 401 once
	failWith   int  // answer admin calls with this status
	adminCalls int
	lastAuth   string
	srv        *httptest.Server
}

func newKC(t *testing.T) *fakeKC {
	t.Helper()
	f := &fakeKC{orgs: map[string]map[string]any{}, members: map[string]map[string]bool{}, users: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/venture/protocol/openid-connect/token", f.token)
	mux.HandleFunc("/admin/realms/venture/organizations", f.orgsCollection)
	mux.HandleFunc("/admin/realms/venture/organizations/", f.orgItem)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKC) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.tokenCalls++
	f.mu.Unlock()
	if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "venture-service" || r.Form.Get("client_secret") != "s3cret" {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized_client"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", f.tokenCalls), "expires_in": 60})
}

func (f *fakeKC) admin(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adminCalls++
	f.lastAuth = r.Header.Get("Authorization")
	if f.revokeNext {
		f.revokeNext = false
		w.WriteHeader(401)
		return false
	}
	if !strings.HasPrefix(f.lastAuth, "Bearer tok-") {
		w.WriteHeader(401)
		return false
	}
	if f.failWith != 0 {
		w.WriteHeader(f.failWith)
		return false
	}
	return true
}

func (f *fakeKC) orgsCollection(w http.ResponseWriter, r *http.Request) {
	if !f.admin(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		out := []map[string]any{}
		q := r.URL.Query().Get("q")
		for _, o := range f.orgs {
			if k, v, ok := strings.Cut(q, ":"); ok {
				attrs, _ := o["attributes"].(map[string]any)
				vals, _ := attrs[k].([]any)
				match := false
				for _, x := range vals {
					match = match || x == v
				}
				if !match {
					continue
				}
			}
			out = append(out, o)
		}
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, o := range f.orgs {
			if o["alias"] == body["alias"] || o["name"] == body["name"] {
				w.WriteHeader(409)
				return
			}
		}
		id := uuid.NewString()
		body["id"] = id
		f.orgs[id] = body
		f.members[id] = map[string]bool{}
		w.Header().Set("Location", f.srv.URL+"/admin/realms/venture/organizations/"+id)
		w.WriteHeader(201)
	}
}

func (f *fakeKC) orgItem(w http.ResponseWriter, r *http.Request) {
	if !f.admin(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rest := strings.TrimPrefix(r.URL.Path, "/admin/realms/venture/organizations/")
	id, sub, _ := strings.Cut(rest, "/")
	if _, ok := f.orgs[id]; !ok {
		w.WriteHeader(404)
		return
	}
	switch {
	case sub == "members" && r.Method == http.MethodPost:
		raw, _ := io.ReadAll(r.Body)
		var user string
		_ = json.Unmarshal(raw, &user)
		if !f.users[user] {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"errorMessage":"User not found"}`))
			return
		}
		if f.members[id][user] {
			w.WriteHeader(409)
			return
		}
		f.members[id][user] = true
		w.WriteHeader(201)
	case sub == "" && r.Method == http.MethodDelete:
		delete(f.orgs, id)
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}

func (f *fakeKC) client() *keycloak.Client {
	return &keycloak.Client{BaseURL: f.srv.URL, Realm: "venture", ClientID: "venture-service", ClientSecret: "s3cret"}
}

func tenant(slug string) ports.TenantInfo {
	return ports.TenantInfo{ID: uuid.New(), Slug: slug, Name: "Kerala Spice Exports"}
}

func TestEnsureCreatesOrganizationWithTenantAttributeAndMember(t *testing.T) {
	kc := newKC(t)
	founder := uuid.NewString()
	kc.users[founder] = true
	tn := tenant("kerala-spice")

	ref, err := kc.client().EnsureOrganization(ctx, tn, founder)
	if err != nil || ref == "" {
		t.Fatalf("ensure: %q %v", ref, err)
	}
	o := kc.orgs[string(ref)]
	if o["alias"] != "kerala-spice" || o["enabled"] != true {
		t.Fatalf("org = %v", o)
	}
	attrs := o["attributes"].(map[string]any)["tenant_id"].([]any)
	if attrs[0] != tn.ID.String() {
		t.Fatalf("tenant attribute = %v", attrs)
	}
	if !kc.members[string(ref)][founder] {
		t.Fatal("founder was not added as a member")
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	kc := newKC(t)
	founder := uuid.NewString()
	kc.users[founder] = true
	tn := tenant("kerala-spice")
	c := kc.client()
	a, err := c.EnsureOrganization(ctx, tn, founder)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.EnsureOrganization(ctx, tn, founder) // membership 409 must be treated as success
	if err != nil || a != b {
		t.Fatalf("second ensure: %q vs %q, %v", a, b, err)
	}
	if len(kc.orgs) != 1 {
		t.Fatalf("created %d organizations", len(kc.orgs))
	}
}

func TestAliasOwnedByAnotherTenantIsNotHijacked(t *testing.T) {
	kc := newKC(t)
	f1, f2 := uuid.NewString(), uuid.NewString()
	kc.users[f1], kc.users[f2] = true, true
	c := kc.client()
	a := tenant("same-slug")
	if _, err := c.EnsureOrganization(ctx, a, f1); err != nil {
		t.Fatal(err)
	}
	b := tenant("same-slug")
	_, err := c.EnsureOrganization(ctx, b, f2)
	if err == nil || !strings.Contains(err.Error(), "already used by another business") {
		t.Fatalf("alias collision: %v", err)
	}
	if kc.members[firstID(kc)][f2] {
		t.Fatal("the second tenant's founder was added to the first tenant's organization")
	}
}

func firstID(kc *fakeKC) string {
	for id := range kc.orgs {
		return id
	}
	return ""
}

func TestUnknownFounderIsAPermanentError(t *testing.T) {
	kc := newKC(t)
	_, err := kc.client().EnsureOrganization(ctx, tenant("kerala-spice"), uuid.NewString())
	if err == nil || errors.Is(err, ports.ErrTransient) || !strings.Contains(err.Error(), "not a user of realm") {
		t.Fatalf("unknown founder: %v", err)
	}
	if _, err := kc.client().EnsureOrganization(ctx, tenant("kerala-spice"), "not-a-uuid"); err == nil {
		t.Fatal("non-uuid subject accepted")
	}
}

func TestRemoveIsScopedToTheTenantsOwnOrganization(t *testing.T) {
	kc := newKC(t)
	f1, f2 := uuid.NewString(), uuid.NewString()
	kc.users[f1], kc.users[f2] = true, true
	c := kc.client()
	a, b := tenant("tenant-a"), tenant("tenant-b")
	refA, _ := c.EnsureOrganization(ctx, a, f1)
	refB, _ := c.EnsureOrganization(ctx, b, f2)

	// Tenant A trying to delete using tenant B's ref must not touch B.
	if err := c.RemoveOrganization(ctx, a, refB); err == nil {
		t.Fatal("removing with another tenant's ref succeeded")
	}
	if _, ok := kc.orgs[string(refB)]; !ok {
		t.Fatal("tenant B's organization was deleted")
	}
	if err := c.RemoveOrganization(ctx, a, refA); err != nil {
		t.Fatal(err)
	}
	if _, ok := kc.orgs[string(refA)]; ok {
		t.Fatal("tenant A's organization still exists")
	}
	if err := c.RemoveOrganization(ctx, a, refA); err != nil {
		t.Fatalf("second removal must succeed: %v", err)
	}
	// With an empty ref (a step that failed before returning one) it still finds and removes by tenant id.
	if err := c.RemoveOrganization(ctx, b, ""); err != nil || len(kc.orgs) != 0 {
		t.Fatalf("remove by tenant id: %v, %d left", err, len(kc.orgs))
	}
}

func TestTokenIsCachedAndRefreshedOn401(t *testing.T) {
	kc := newKC(t)
	f := uuid.NewString()
	kc.users[f] = true
	c := kc.client()
	for i := 0; i < 3; i++ {
		if _, err := c.EnsureOrganization(ctx, tenant(fmt.Sprintf("slug-%d", i)), f); err != nil {
			t.Fatal(err)
		}
	}
	if kc.tokenCalls != 1 {
		t.Fatalf("token requested %d times, want 1 (cached)", kc.tokenCalls)
	}
	kc.revokeNext = true // e.g. realm keys rotated: one 401, then the adapter must re-authenticate and succeed
	if _, err := c.EnsureOrganization(ctx, tenant("after-401"), f); err != nil {
		t.Fatalf("after a 401: %v", err)
	}
	if kc.tokenCalls != 2 {
		t.Fatalf("token calls = %d, want 2", kc.tokenCalls)
	}
}

func TestTokenExpiryTriggersRenewal(t *testing.T) {
	kc := newKC(t)
	f := uuid.NewString()
	kc.users[f] = true
	now := time.Now()
	c := kc.client()
	c.Now = func() time.Time { return now }
	_, _ = c.EnsureOrganization(ctx, tenant("one-slug"), f)
	now = now.Add(45 * time.Second) // within 30s of the 60s expiry
	_, _ = c.EnsureOrganization(ctx, tenant("two-slug"), f)
	if kc.tokenCalls != 2 {
		t.Fatalf("token calls = %d, want 2", kc.tokenCalls)
	}
}

func TestErrorClassification(t *testing.T) {
	kc := newKC(t)
	f := uuid.NewString()
	kc.users[f] = true
	c := kc.client()
	for status, transient := range map[int]bool{500: true, 502: true, 503: true, 429: true, 400: false, 403: false} {
		kc.failWith = status
		_, err := c.EnsureOrganization(ctx, tenant("kerala-spice"), f)
		if err == nil || errors.Is(err, ports.ErrTransient) != transient {
			t.Errorf("status %d: transient=%v err=%v", status, errors.Is(err, ports.ErrTransient), err)
		}
	}
	// An unreachable server is transient.
	dead := &keycloak.Client{BaseURL: "http://127.0.0.1:1", Realm: "venture", ClientID: "a", ClientSecret: "b"}
	if _, err := dead.EnsureOrganization(ctx, tenant("kerala-spice"), f); !errors.Is(err, ports.ErrTransient) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestBadCredentialsAndConfigAreReportedWithoutLeakingTheSecret(t *testing.T) {
	kc := newKC(t)
	f := uuid.NewString()
	c := kc.client()
	c.ClientSecret = "wrong-secret"
	_, err := c.EnsureOrganization(ctx, tenant("kerala-spice"), f)
	if err == nil || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("bad credentials: %v", err)
	}
	if _, err := (&keycloak.Client{}).EnsureOrganization(ctx, tenant("kerala-spice"), f); err == nil {
		t.Error("empty config accepted")
	}
	if _, err := kc.client().EnsureOrganization(ctx, ports.TenantInfo{Slug: "x"}, f); err == nil {
		t.Error("nil tenant id accepted")
	}
}
