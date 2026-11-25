// Package collabsvc turns a business's stored connections into live provider clients, and is how a connection is made.
//
// Connecting is deliberately strict: the submitted credentials are used to build the real client and verified against
// the provider BEFORE anything is stored, so a typo or a revoked token is reported to the person who just typed it, not
// discovered later when an agent's call fails. Only a connection that passed its check is saved, and its credentials are
// sealed with the vault first. Reads decrypt on demand and cache the built client (and with it the provider's access
// token) until the connection changes.
package collabsvc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/connectors/googlecalendar"
	"github.com/mrbiz/venture-os/internal/connectors/notion"
	"github.com/mrbiz/venture-os/internal/connectors/zoom"
	"github.com/mrbiz/venture-os/internal/httpx"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/vault"
)

// Providers are the supported provider names.
var Providers = []string{"google", "zoom", "notion"}

// fields are the credentials and settings each provider accepts. Anything else is refused.
var fields = map[string]struct{ secrets, config []string }{
	"google": {secrets: []string{"client_id", "client_secret", "refresh_token"}, config: []string{"calendar_id"}},
	"zoom":   {secrets: []string{"account_id", "client_id", "client_secret"}, config: []string{"user_id"}},
	"notion": {secrets: []string{"token"}, config: []string{"database_id", "title_property"}},
}

// Fields reports which secret and setting names a provider takes (for forms and documentation).
func Fields(provider string) (secrets, config []string, ok bool) {
	f, ok := fields[provider]
	return f.secrets, f.config, ok
}

// Store is the persistence the registry needs (implemented by *store.PG).
type Store interface {
	Connection(ctx context.Context, tenant uuid.UUID, provider string) (store.Connection, vault.Sealed, error)
	Connections(ctx context.Context, tenant uuid.UUID) ([]store.Connection, error)
	SaveConnection(ctx context.Context, tenant uuid.UUID, c store.Connection, sealed vault.Sealed) error
	DeleteConnection(ctx context.Context, tenant uuid.UUID, provider string) error
}

// Endpoints override the providers' base URLs; they exist so tests can point the clients at fakes.
type Endpoints struct {
	GoogleAPI, GoogleOAuth, ZoomAPI, ZoomOAuth, NotionAPI string
	Tune                                                  func(*httpx.Config)
	Now                                                   func() time.Time
}

type checker interface {
	Check(ctx context.Context) error
}

type built struct {
	updated time.Time
	client  any // *googlecalendar.Client, *zoom.Client or *notion.Client
}

// Registry implements collab.Provider over stored connections.
type Registry struct {
	Store     Store
	Vault     *vault.Vault
	Endpoints Endpoints

	mu    sync.Mutex
	cache map[string]built
}

var _ collab.Provider = (*Registry)(nil)

func (r *Registry) build(provider string, config, secret map[string]string) (any, error) {
	e := r.Endpoints
	switch provider {
	case "google":
		return googlecalendar.New(googlecalendar.Config{ClientID: secret["client_id"], ClientSecret: secret["client_secret"], RefreshToken: secret["refresh_token"],
			CalendarID: config["calendar_id"], APIBase: e.GoogleAPI, OAuthBase: e.GoogleOAuth, Tune: e.Tune, Now: e.Now})
	case "zoom":
		return zoom.New(zoom.Config{AccountID: secret["account_id"], ClientID: secret["client_id"], ClientSecret: secret["client_secret"],
			UserID: config["user_id"], APIBase: e.ZoomAPI, OAuthBase: e.ZoomOAuth, Tune: e.Tune, Now: e.Now})
	case "notion":
		return notion.New(notion.Config{Token: secret["token"], DatabaseID: config["database_id"], TitleProperty: config["title_property"], APIBase: e.NotionAPI, Tune: e.Tune})
	}
	return nil, fmt.Errorf("%w: unknown provider %q", collab.ErrInvalid, provider)
}

func allowed(given map[string]string, names []string, what string) error {
	ok := map[string]bool{}
	for _, n := range names {
		ok[n] = true
	}
	for k, v := range given {
		if !ok[k] {
			return fmt.Errorf("%w: %s %q is not recognised (expected %s)", collab.ErrInvalid, what, k, strings.Join(names, ", "))
		}
		if utf8.RuneCountInString(v) > 4096 || strings.ContainsRune(v, 0) {
			return fmt.Errorf("%w: %s %q is too long", collab.ErrInvalid, what, k)
		}
	}
	return nil
}

// Connect verifies the credentials against the provider and, only if they work, stores the connection with its
// credentials sealed. The plaintext secret is never returned or logged.
func (r *Registry) Connect(ctx context.Context, tenant uuid.UUID, provider string, config, secret map[string]string, by string) error {
	f, ok := fields[provider]
	if !ok {
		return fmt.Errorf("%w: provider must be one of %s", collab.ErrInvalid, strings.Join(Providers, ", "))
	}
	if err := allowed(secret, f.secrets, "credential"); err != nil {
		return err
	}
	if err := allowed(config, f.config, "setting"); err != nil {
		return err
	}
	for _, k := range f.secrets {
		if strings.TrimSpace(secret[k]) == "" {
			return fmt.Errorf("%w: credential %q is required", collab.ErrInvalid, k)
		}
	}
	client, err := r.build(provider, config, secret)
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := client.(checker).Check(checkCtx); err != nil {
		return err
	}
	sealed, err := r.Vault.Seal(tenant, provider, mustJSON(secret))
	if err != nil {
		return err
	}
	if err := r.Store.SaveConnection(ctx, tenant, store.Connection{Provider: provider, Config: config, SecretName: provider, ConnectedBy: by}, sealed); err != nil {
		return err
	}
	r.forget(tenant, provider)
	return nil
}

// Disconnect removes a connection and destroys its stored credentials.
func (r *Registry) Disconnect(ctx context.Context, tenant uuid.UUID, provider string) error {
	err := r.Store.DeleteConnection(ctx, tenant, provider)
	r.forget(tenant, provider)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %s is not connected", collab.ErrNotConnected, provider)
	}
	return err
}

// List returns the tenant's connections without credentials.
func (r *Registry) List(ctx context.Context, tenant uuid.UUID) ([]store.Connection, error) {
	cs, err := r.Store.Connections(ctx, tenant)
	sort.Slice(cs, func(i, j int) bool { return cs[i].Provider < cs[j].Provider })
	return cs, err
}

func key(tenant uuid.UUID, provider string) string { return tenant.String() + "|" + provider }

func (r *Registry) forget(tenant uuid.UUID, provider string) {
	r.mu.Lock()
	delete(r.cache, key(tenant, provider))
	r.mu.Unlock()
}

// client returns the built client for a connection, or nil if the business has not connected the provider.
func (r *Registry) client(ctx context.Context, tenant uuid.UUID, provider string) (any, error) {
	conn, sealed, err := r.Store.Connection(ctx, tenant, provider)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k := key(tenant, provider)
	r.mu.Lock()
	if b, ok := r.cache[k]; ok && b.updated.Equal(conn.UpdatedAt) {
		r.mu.Unlock()
		return b.client, nil
	}
	r.mu.Unlock()
	plain, err := r.Vault.Open(tenant, provider, sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: the stored %s credentials cannot be read; reconnect %s", collab.ErrAuth, provider, provider)
	}
	secret, err := parseSecret(plain)
	if err != nil {
		return nil, err
	}
	c, err := r.build(provider, conn.Config, secret)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]built{}
	}
	r.cache[k] = built{updated: conn.UpdatedAt, client: c}
	r.mu.Unlock()
	return c, nil
}

// For returns the services a business has connected (collab.Provider). A provider that is not connected is a nil
// service, not an error.
func (r *Registry) For(ctx context.Context, tenant string) (collab.Services, error) {
	id, err := uuid.Parse(tenant)
	if err != nil {
		return collab.Services{}, fmt.Errorf("%w: tenant id", collab.ErrInvalid)
	}
	var s collab.Services
	if c, err := r.client(ctx, id, "google"); err != nil {
		return s, err
	} else if c != nil {
		s.Calendar = c.(*googlecalendar.Client)
	}
	if c, err := r.client(ctx, id, "zoom"); err != nil {
		return s, err
	} else if c != nil {
		s.Zoom = c.(*zoom.Client)
	}
	if c, err := r.client(ctx, id, "notion"); err != nil {
		return s, err
	} else if c != nil {
		s.Notes = c.(*notion.Client)
	}
	return s, nil
}
