package collabsvc_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/collabsvc"
	"github.com/mrbiz/venture-os/internal/connectors/fakeapi"
	"github.com/mrbiz/venture-os/internal/httpx"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/vault"
)

var ctx = context.Background()

// memStore is a recording in-memory Store; it keeps exactly what a real one would keep.
type memStore struct {
	mu    sync.Mutex
	conns map[string]store.Connection
	seal  map[string]vault.Sealed
	saves int
}

func newMem() *memStore {
	return &memStore{conns: map[string]store.Connection{}, seal: map[string]vault.Sealed{}}
}

func k(t uuid.UUID, p string) string { return t.String() + "|" + p }

func (m *memStore) Connection(_ context.Context, t uuid.UUID, p string) (store.Connection, vault.Sealed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[k(t, p)]
	if !ok {
		return store.Connection{}, vault.Sealed{}, store.ErrNotFound
	}
	return c, m.seal[k(t, p)], nil
}
func (m *memStore) Connections(_ context.Context, t uuid.UUID) ([]store.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Connection
	for key, c := range m.conns {
		if strings.HasPrefix(key, t.String()) {
			out = append(out, c)
		}
	}
	return out, nil
}
func (m *memStore) SaveConnection(_ context.Context, t uuid.UUID, c store.Connection, s vault.Sealed) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	c.Status, c.UpdatedAt = "ACTIVE", time.Now().Add(time.Duration(m.saves)*time.Second)
	m.conns[k(t, c.Provider)], m.seal[k(t, c.Provider)] = c, s
	return nil
}
func (m *memStore) DeleteConnection(_ context.Context, t uuid.UUID, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.conns[k(t, p)]; !ok {
		return store.ErrNotFound
	}
	delete(m.conns, k(t, p))
	delete(m.seal, k(t, p))
	return nil
}

type env struct {
	reg    *collabsvc.Registry
	mem    *memStore
	google *fakeapi.Google
	zoom   *fakeapi.Zoom
	notion *fakeapi.Notion
}

const dbID = "a1b2c3d4-0000-4000-8000-000000000001"

func setup(t *testing.T) env {
	t.Helper()
	v, _ := vault.New(map[int][]byte{1: bytes.Repeat([]byte{9}, 32)}, 1)
	e := env{mem: newMem(), google: fakeapi.NewGoogle("gid", "gsecret", "grefresh"), zoom: fakeapi.NewZoom("acct", "zid", "zsecret"), notion: fakeapi.NewNotion("ntok", dbID, "Name")}
	t.Cleanup(func() { e.google.Close(); e.zoom.Close(); e.notion.Close() })
	e.reg = &collabsvc.Registry{Store: e.mem, Vault: v, Endpoints: collabsvc.Endpoints{
		GoogleAPI: e.google.URL(), GoogleOAuth: e.google.URL(), ZoomAPI: e.zoom.URL(), ZoomOAuth: e.zoom.URL(), NotionAPI: e.notion.URL() + "/v1",
		Tune: func(h *httpx.Config) { h.Sleep = func(context.Context, time.Duration) error { return nil } },
	}}
	return e
}

var (
	goodGoogle = map[string]string{"client_id": "gid", "client_secret": "gsecret", "refresh_token": "grefresh"}
	goodZoom   = map[string]string{"account_id": "acct", "client_id": "zid", "client_secret": "zsecret"}
	goodNotion = map[string]string{"token": "ntok"}
	notionCfg  = map[string]string{"database_id": dbID}
)

func TestConnectVerifiesThenStoresOnlySealedCredentials(t *testing.T) {
	e := setup(t)
	tn := uuid.New()
	for _, c := range []struct {
		provider string
		cfg, sec map[string]string
	}{{"google", map[string]string{"calendar_id": "primary"}, goodGoogle}, {"zoom", nil, goodZoom}, {"notion", notionCfg, goodNotion}} {
		if err := e.reg.Connect(ctx, tn, c.provider, c.cfg, c.sec, "founder"); err != nil {
			t.Fatalf("%s: %v", c.provider, err)
		}
	}
	if e.mem.saves != 3 {
		t.Fatalf("%d saves", e.mem.saves)
	}
	for p, secret := range map[string]string{"google": "grefresh", "zoom": "zsecret", "notion": "ntok"} {
		s := e.mem.seal[k(tn, p)]
		if bytes.Contains(s.Ciphertext, []byte(secret)) || bytes.Contains(s.Nonce, []byte(secret)) {
			t.Fatalf("%s: the credential is stored in the clear", p)
		}
		if c := e.mem.conns[k(tn, p)]; len(c.Config["refresh_token"]) != 0 || c.SecretName != p {
			t.Fatalf("%s: the connection row must hold no credentials: %+v", p, c)
		}
	}
	list, _ := e.reg.List(ctx, tn)
	if len(list) != 3 || list[0].Provider != "google" || list[1].Provider != "notion" || list[2].Provider != "zoom" {
		t.Fatalf("list = %+v", list)
	}
}

func TestBadCredentialsAreReportedAndNothingIsStored(t *testing.T) {
	e := setup(t)
	tn := uuid.New()
	cases := map[string]struct {
		provider string
		cfg, sec map[string]string
		want     error
	}{
		"google wrong secret": {"google", nil, map[string]string{"client_id": "gid", "client_secret": "WRONG", "refresh_token": "grefresh"}, collab.ErrAuth},
		"google bad refresh":  {"google", nil, map[string]string{"client_id": "gid", "client_secret": "gsecret", "refresh_token": "revoked"}, collab.ErrAuth},
		"zoom wrong secret":   {"zoom", nil, map[string]string{"account_id": "acct", "client_id": "zid", "client_secret": "WRONG"}, collab.ErrAuth},
		"notion wrong token":  {"notion", notionCfg, map[string]string{"token": "WRONG"}, collab.ErrAuth},
		"notion unshared db":  {"notion", map[string]string{"database_id": "ffffffff-0000-4000-8000-000000000001"}, goodNotion, collab.ErrNotFound},
		"notion no database":  {"notion", nil, goodNotion, collab.ErrInvalid},
		"missing credential":  {"zoom", nil, map[string]string{"account_id": "acct", "client_id": "zid"}, collab.ErrInvalid},
		"blank credential":    {"zoom", nil, map[string]string{"account_id": "acct", "client_id": "zid", "client_secret": "  "}, collab.ErrInvalid},
		"unknown credential":  {"notion", notionCfg, map[string]string{"token": "ntok", "admin": "yes"}, collab.ErrInvalid},
		"unknown setting":     {"google", map[string]string{"evil": "x"}, goodGoogle, collab.ErrInvalid},
		"unknown provider":    {"slack", nil, goodNotion, collab.ErrInvalid},
		"huge value":          {"notion", notionCfg, map[string]string{"token": strings.Repeat("x", 5000)}, collab.ErrInvalid},
	}
	for name, c := range cases {
		if err := e.reg.Connect(ctx, tn, c.provider, c.cfg, c.sec, "f"); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", name, c.want, err)
		}
	}
	if e.mem.saves != 0 {
		t.Fatalf("%d connections were stored although every check failed", e.mem.saves)
	}
}

func TestForBuildsOnlyWhatIsConnectedAndCachesTheClient(t *testing.T) {
	e := setup(t)
	tn := uuid.New()
	s, err := e.reg.For(ctx, tn.String())
	if err != nil || s.Calendar != nil || s.Zoom != nil || s.Notes != nil {
		t.Fatalf("an unconnected business has no services: %+v %v", s, err)
	}
	must(t, e.reg.Connect(ctx, tn, "zoom", nil, goodZoom, "f"))
	s, _ = e.reg.For(ctx, tn.String())
	if s.Zoom == nil || s.Calendar != nil || s.Notes != nil {
		t.Fatalf("only zoom is connected: %+v", s)
	}
	// Using the service twice must reuse one OAuth token (the client, and its token cache, survive between calls).
	in := collab.MeetingInput{Title: "t", Start: time.Now().Add(time.Hour), Duration: 30 * time.Minute}
	for i := 0; i < 3; i++ {
		s, _ := e.reg.For(ctx, tn.String())
		if _, err := s.Zoom.Schedule(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	tokens := 0
	for _, c := range e.zoom.Calls() {
		if c.Path == "/oauth/token" {
			tokens++
		}
	}
	if tokens != 2 { // one for the Check during Connect, one cached for the three calls
		t.Fatalf("%d token requests; the built client must be cached", tokens)
	}
	if _, err := e.reg.For(ctx, "not-a-uuid"); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("bad tenant id: %v", err)
	}
}

func TestTenantsDoNotShareConnectionsAndAStolenRowIsUnreadable(t *testing.T) {
	e := setup(t)
	a, b := uuid.New(), uuid.New()
	must(t, e.reg.Connect(ctx, a, "notion", notionCfg, goodNotion, "f"))
	if s, _ := e.reg.For(ctx, b.String()); s.Notes != nil {
		t.Fatal("tenant B got tenant A's Notion")
	}
	// An attacker with write access copies A's sealed secret into B's row.
	e.mem.conns[k(b, "notion")] = e.mem.conns[k(a, "notion")]
	e.mem.seal[k(b, "notion")] = e.mem.seal[k(a, "notion")]
	_, err := e.reg.For(ctx, b.String())
	if !errors.Is(err, collab.ErrAuth) || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("a secret moved to another tenant must not decrypt: %v", err)
	}
}

func TestReconnectingReplacesAndDisconnectingForgets(t *testing.T) {
	e := setup(t)
	tn := uuid.New()
	must(t, e.reg.Connect(ctx, tn, "notion", notionCfg, goodNotion, "f"))
	s1, _ := e.reg.For(ctx, tn.String())
	must(t, e.reg.Connect(ctx, tn, "notion", notionCfg, goodNotion, "f2"))
	s2, _ := e.reg.For(ctx, tn.String())
	if s1.Notes == s2.Notes {
		t.Fatal("a reconnect must rebuild the client so new credentials take effect")
	}
	must(t, e.reg.Disconnect(ctx, tn, "notion"))
	if s, _ := e.reg.For(ctx, tn.String()); s.Notes != nil {
		t.Fatal("a disconnected provider is still served from the cache")
	}
	if err := e.reg.Disconnect(ctx, tn, "notion"); !errors.Is(err, collab.ErrNotConnected) {
		t.Fatalf("disconnecting twice: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
