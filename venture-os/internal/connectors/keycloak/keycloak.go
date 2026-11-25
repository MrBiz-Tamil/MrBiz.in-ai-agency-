// Package keycloak implements ports.IdentityProvider on the Keycloak Admin REST API (Organizations, Keycloak 26+).
//
// A tenant maps to exactly one Keycloak organization, found by its `tenant_id` attribute (never by name or alias,
// which anyone could pick), so the adapter can only ever touch organizations it created for that tenant.
//
// The control plane authenticates with a confidential service-account client (client_credentials grant); the realm
// import grants that service account manage-realm and manage-users on realm-management.
package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/ports"
)

const (
	attrTenantID = "tenant_id"
	maxBody      = 1 << 20
)

// Client talks to one realm of a Keycloak server.
type Client struct {
	// BaseURL is the server root as reachable from this process, e.g. http://keycloak:8080.
	BaseURL string
	Realm   string
	// ClientID / ClientSecret are the service-account client's credentials.
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	Now          func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

var _ ports.IdentityProvider = (*Client)(nil)

type org struct {
	ID         string              `json:"id,omitempty"`
	Name       string              `json:"name"`
	Alias      string              `json:"alias"`
	Enabled    bool                `json:"enabled"`
	Attributes map[string][]string `json:"attributes,omitempty"`
	Domains    []domain            `json:"domains,omitempty"`
}

type domain struct {
	Name     string `json:"name"`
	Verified bool   `json:"verified"`
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (c *Client) validate() error {
	if c.BaseURL == "" || c.Realm == "" || c.ClientID == "" || c.ClientSecret == "" {
		return errors.New("keycloak: BaseURL, Realm, ClientID and ClientSecret are required")
	}
	return nil
}

// ───────────────────────────── token ─────────────────────────────

func (c *Client) bearer(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.now().Before(c.expires.Add(-30*time.Second)) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.ClientID}, "client_secret": {c.ClientSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/realms/"+url.PathEscape(c.Realm)+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpc().Do(req)
	if err != nil {
		return "", transient("token request", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		return "", statusError("token request", resp.StatusCode, raw)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil || tr.AccessToken == "" {
		return "", errors.New("keycloak: token response had no access_token")
	}
	c.token, c.expires = tr.AccessToken, c.now().Add(time.Duration(tr.ExpiresIn)*time.Second)
	return c.token, nil
}

// ───────────────────────────── HTTP plumbing ─────────────────────────────

func transient(what string, err error) error {
	return fmt.Errorf("keycloak: %s: %w: %w", what, err, ports.ErrTransient)
}

func statusError(what string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	err := fmt.Errorf("keycloak: %s: HTTP %d: %s", what, status, msg)
	if status == http.StatusTooManyRequests || status >= 500 {
		return fmt.Errorf("%w: %w", err, ports.ErrTransient)
	}
	return err
}

// do performs an authenticated admin call, retrying once with a fresh token on 401.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (int, http.Header, []byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, nil, nil, err
		}
	}
	u := strings.TrimRight(c.BaseURL, "/") + "/admin/realms/" + url.PathEscape(c.Realm) + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.bearer(ctx, attempt > 0)
		if err != nil {
			return 0, nil, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.httpc().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, nil, ctx.Err()
			}
			return 0, nil, nil, transient(method+" "+path, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue // the cached token may have been revoked or the realm keys rotated
		}
		return resp.StatusCode, resp.Header, raw, nil
	}
}

// ───────────────────────────── IdentityProvider ─────────────────────────────

func orgName(t ports.TenantInfo) string { return fmt.Sprintf("%s (%s)", t.Name, t.Slug) }

// findByTenant returns the organization carrying this tenant's id, or nil.
func (c *Client) findByTenant(ctx context.Context, tenant uuid.UUID) (*org, error) {
	status, _, raw, err := c.do(ctx, http.MethodGet, "/organizations", url.Values{
		"q": {attrTenantID + ":" + tenant.String()}, "briefRepresentation": {"false"}, "max": {"5"},
	}, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError("list organizations", status, raw)
	}
	var orgs []org
	if err := json.Unmarshal(raw, &orgs); err != nil {
		return nil, fmt.Errorf("keycloak: decode organizations: %w", err)
	}
	for i := range orgs {
		for _, v := range orgs[i].Attributes[attrTenantID] {
			if v == tenant.String() {
				return &orgs[i], nil
			}
		}
	}
	return nil, nil
}

// EnsureOrganization creates the tenant's organization (idempotently) and makes founderSub a member.
func (c *Client) EnsureOrganization(ctx context.Context, t ports.TenantInfo, founderSub string) (ports.Ref, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	if t.ID == uuid.Nil || t.Slug == "" {
		return "", errors.New("keycloak: tenant id and slug are required")
	}
	if _, err := uuid.Parse(founderSub); err != nil {
		return "", fmt.Errorf("keycloak: founder subject %q is not a Keycloak user id", founderSub)
	}

	existing, err := c.findByTenant(ctx, t.ID)
	if err != nil {
		return "", err
	}
	id := ""
	if existing != nil {
		id = existing.ID
	} else {
		status, hdr, raw, err := c.do(ctx, http.MethodPost, "/organizations", nil, org{
			Name: orgName(t), Alias: t.Slug, Enabled: true,
			Attributes: map[string][]string{attrTenantID: {t.ID.String()}},
			Domains:    []domain{{Name: t.Slug + ".tenants.venture.invalid"}},
		})
		if err != nil {
			return "", err
		}
		switch {
		case status == http.StatusCreated:
			id = lastSegment(hdr.Get("Location"))
		case status == http.StatusConflict:
			// Lost a race with a concurrent attempt for this tenant, or the alias/name belongs to someone else.
			if again, ferr := c.findByTenant(ctx, t.ID); ferr == nil && again != nil {
				id = again.ID
			} else {
				return "", fmt.Errorf("keycloak: organization alias %q is already used by another business", t.Slug)
			}
		default:
			return "", statusError("create organization", status, raw)
		}
	}
	if id == "" {
		return "", errors.New("keycloak: organization id missing from the create response")
	}

	status, _, raw, err := c.do(ctx, http.MethodPost, "/organizations/"+url.PathEscape(id)+"/members", nil, founderSub)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusCreated, http.StatusNoContent, http.StatusConflict: // 409: already a member
	case http.StatusNotFound, http.StatusBadRequest:
		return "", fmt.Errorf("keycloak: founder %s is not a user of realm %q: %s", founderSub, c.Realm, strings.TrimSpace(string(raw)))
	default:
		return "", statusError("add founder to organization", status, raw)
	}
	return ports.Ref(id), nil
}

// RemoveOrganization deletes the tenant's organization. It never deletes an organization that does not carry this
// tenant's id, whatever ref it is given, and succeeds if it is already gone.
func (c *Client) RemoveOrganization(ctx context.Context, t ports.TenantInfo, ref ports.Ref) error {
	if err := c.validate(); err != nil {
		return err
	}
	if t.ID == uuid.Nil {
		return errors.New("keycloak: tenant id is required")
	}
	existing, err := c.findByTenant(ctx, t.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil // nothing of ours to remove (also covers a stale or foreign ref)
	}
	if ref != "" && string(ref) != existing.ID {
		return fmt.Errorf("keycloak: ref %q is not this tenant's organization", ref)
	}
	status, _, raw, err := c.do(ctx, http.MethodDelete, "/organizations/"+url.PathEscape(existing.ID), nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK && status != http.StatusNotFound {
		return statusError("delete organization", status, raw)
	}
	return nil
}

func lastSegment(location string) string {
	location = strings.TrimRight(location, "/")
	if i := strings.LastIndex(location, "/"); i >= 0 {
		return location[i+1:]
	}
	return ""
}
