// Package erpnext implements ports.ERP on the Frappe/ERPNext REST API.
//
// Each tenant gets one ERPNext Company on the shared site. The company is created from ERPNext's standard template
// (which makes the five root accounts and the cost centres ERPNext needs), then the blueprint's chart of accounts is
// created beneath those roots, with ERPNext account types set for the accounts the finance engine posts to
// (bank, receivable, payable, tax).
//
// A company is recognised as ours by a marker in its description ("venture-tenant:<uuid>"), never by name, so the
// adapter cannot adopt or delete a company it did not create for that tenant.
//
// Not covered: ERPNext tax templates (they need per-company tax accounts and are a separate step), opening
// balances (the Venture OS ledger owns balances), and deleting a company that already has transactions (ERPNext
// itself refuses; the error is returned so the provisioner reports it rather than hiding it).
package erpnext

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
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/ports"
)

const (
	markerPrefix = "venture-tenant:"
	maxBody      = 4 << 20
)

// Client talks to one Frappe site.
type Client struct {
	// BaseURL is the nginx front of ERPNext reachable from this process, e.g. http://erp-frontend:8080.
	BaseURL string
	// Site is the Frappe site name, sent as the Host header (frappe routes by Host), e.g. erp.localhost.
	Site string
	// APIKey / APISecret authenticate as a dedicated, least-privilege ERPNext user.
	APIKey, APISecret string
	HTTP              *http.Client
}

var _ ports.ERP = (*Client)(nil)

func (c *Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 150 * time.Second} // company creation builds the whole standard chart
}

func (c *Client) validate() error {
	if c.BaseURL == "" || c.Site == "" || c.APIKey == "" || c.APISecret == "" {
		return errors.New("erpnext: BaseURL, Site, APIKey and APISecret are required")
	}
	return nil
}

// ───────────────────────────── HTTP plumbing ─────────────────────────────

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("erpnext: HTTP %d: %s", e.Status, e.Message) }

func (e *apiError) Is(target error) bool {
	return target == ports.ErrTransient && (e.Status == http.StatusTooManyRequests || e.Status >= 500)
}

func frappeMessage(raw []byte) string {
	var e struct {
		ExcType   string `json:"exc_type"`
		Exception string `json:"exception"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil {
		switch {
		case e.Exception != "":
			return trimMsg(e.Exception)
		case e.Message != "":
			return trimMsg(e.Message)
		case e.ExcType != "":
			return e.ExcType
		}
	}
	return trimMsg(strings.TrimSpace(string(raw)))
}

func trimMsg(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, payload)
	if err != nil {
		return err
	}
	req.Host = c.Site
	req.Header.Set("Authorization", "token "+c.APIKey+":"+c.APISecret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("erpnext: %s %s: %w: %w", method, path, err, ports.ErrTransient)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode >= 300 {
		return &apiError{Status: resp.StatusCode, Message: frappeMessage(raw)}
	}
	if out != nil {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("erpnext: decode response: %w", err)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("erpnext: decode data: %w", err)
		}
	}
	return nil
}

func isStatus(err error, status int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == status
}

func jsonParam(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func listQuery(filters any, fields ...string) url.Values {
	q := url.Values{"filters": {jsonParam(filters)}, "limit_page_length": {"1000"}}
	if len(fields) > 0 {
		q.Set("fields", jsonParam(fields))
	}
	return q
}

// ───────────────────────────── company ─────────────────────────────

type company struct {
	Name        string `json:"name"`
	Description string `json:"company_description"`
}

func marker(tenant uuid.UUID) string { return markerPrefix + tenant.String() }

func (c *Client) findCompany(ctx context.Context, tenant uuid.UUID) (*company, error) {
	var found []company
	err := c.do(ctx, http.MethodGet, "/api/resource/Company",
		listQuery([][]any{{"company_description", "like", "%" + marker(tenant) + "%"}}, "name", "company_description"), nil, &found)
	if err != nil {
		return nil, err
	}
	for i := range found {
		if strings.Contains(found[i].Description, marker(tenant)) {
			return &found[i], nil
		}
	}
	return nil, nil
}

func (c *Client) ensureCurrencyEnabled(ctx context.Context, code string) error {
	var cur struct {
		Enabled int `json:"enabled"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/resource/Currency/"+url.PathEscape(code), nil, nil, &cur); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return fmt.Errorf("erpnext: currency %q is not known to this ERPNext site", code)
		}
		return err
	}
	if cur.Enabled == 1 {
		return nil
	}
	return c.do(ctx, http.MethodPut, "/api/resource/Currency/"+url.PathEscape(code), nil, map[string]any{"enabled": 1}, nil)
}

func (c *Client) countryName(ctx context.Context, iso2 string) (string, error) {
	var rows []struct {
		Name string `json:"name"`
	}
	err := c.do(ctx, http.MethodGet, "/api/resource/Country",
		listQuery([][]any{{"code", "=", strings.ToLower(iso2)}}, "name"), nil, &rows)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("erpnext: no country with ISO code %q on this site", iso2)
	}
	return rows[0].Name, nil
}

// abbreviation builds a short, tenant-unique company abbreviation (ERPNext requires uniqueness): initials of the
// slug words plus three hex digits of the tenant id.
func abbreviation(t ports.TenantInfo) string {
	var initials []rune
	for _, w := range strings.Split(t.Slug, "-") {
		if w != "" && len(initials) < 4 {
			initials = append(initials, rune(strings.ToUpper(w)[0]))
		}
	}
	id := strings.ReplaceAll(t.ID.String(), "-", "")
	return string(initials) + strings.ToUpper(id[:3])
}

// EnsureCompany creates the tenant's company and chart of accounts, or completes it if a previous attempt stopped
// part-way. It never modifies a company that is not marked as this tenant's.
func (c *Client) EnsureCompany(ctx context.Context, t ports.TenantInfo, profile domain.BusinessProfile, acct domain.Accounting) (ports.Ref, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	if t.ID == uuid.Nil {
		return "", errors.New("erpnext: tenant id is required")
	}
	existing, err := c.findCompany(ctx, t.ID)
	if err != nil {
		return "", err
	}
	name := ""
	if existing != nil {
		name = existing.Name
	} else {
		if err := c.ensureCurrencyEnabled(ctx, profile.BaseCurrency); err != nil {
			return "", err
		}
		country, err := c.countryName(ctx, profile.Country)
		if err != nil {
			return "", err
		}
		name, err = c.createCompany(ctx, t, profile, country)
		if err != nil {
			return "", err
		}
	}
	if err := c.applyChart(ctx, name, acct); err != nil {
		return "", err
	}
	return ports.Ref(name), nil
}

func (c *Client) createCompany(ctx context.Context, t ports.TenantInfo, profile domain.BusinessProfile, country string) (string, error) {
	create := func(name string) error {
		return c.do(ctx, http.MethodPost, "/api/resource/Company", nil, map[string]any{
			"company_name":                      name,
			"abbr":                              abbreviation(t),
			"default_currency":                  profile.BaseCurrency,
			"country":                           country,
			"create_chart_of_accounts_based_on": "Standard Template",
			"chart_of_accounts":                 "Standard",
			"company_description":               marker(t.ID) + "\nProvisioned by Venture OS. Do not edit this line.",
		}, nil)
	}
	name := profile.Name
	err := create(name)
	if isStatus(err, http.StatusConflict) {
		// Another company already has this name (or abbreviation). Disambiguate with the slug, once.
		if again, ferr := c.findCompany(ctx, t.ID); ferr == nil && again != nil {
			return again.Name, nil // a concurrent attempt for this tenant won
		}
		name = fmt.Sprintf("%s (%s)", profile.Name, t.Slug)
		err = create(name)
		if isStatus(err, http.StatusConflict) {
			if again, ferr := c.findCompany(ctx, t.ID); ferr == nil && again != nil {
				return again.Name, nil
			}
			return "", fmt.Errorf("erpnext: a company named %q already exists and is not this business's", name)
		}
	}
	if err != nil {
		return "", err
	}
	return name, nil
}

// ───────────────────────────── chart of accounts ─────────────────────────────

var rootType = map[domain.AccountType]string{
	domain.AccountAsset:     "Asset",
	domain.AccountLiability: "Liability",
	domain.AccountEquity:    "Equity",
	domain.AccountRevenue:   "Income",
	domain.AccountExpense:   "Expense",
}

type erpAccount struct {
	Name          string `json:"name"`
	AccountNumber string `json:"account_number"`
}

func (c *Client) applyChart(ctx context.Context, company string, acct domain.Accounting) error {
	// Roots created by the standard template, one per root type.
	roots := map[string]string{}
	for _, rt := range rootType {
		if _, done := roots[rt]; done {
			continue
		}
		var rows []erpAccount
		err := c.do(ctx, http.MethodGet, "/api/resource/Account",
			listQuery([][]any{{"company", "=", company}, {"root_type", "=", rt}, {"is_group", "=", 1}, {"parent_account", "=", ""}}, "name"), nil, &rows)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("erpnext: company %q has no %s root account (is the standard template installed?)", company, rt)
		}
		roots[rt] = rows[0].Name
	}

	// Existing accounts by number, so a re-run creates only what is missing.
	var have []erpAccount
	if err := c.do(ctx, http.MethodGet, "/api/resource/Account",
		listQuery([][]any{{"company", "=", company}, {"account_number", "!=", ""}}, "name", "account_number"), nil, &have); err != nil {
		return err
	}
	nameByCode := map[string]string{}
	for _, a := range have {
		nameByCode[a.AccountNumber] = a.Name
	}

	sys := acct.SystemAccounts
	accountType := map[string]string{sys.Cash: "Bank", sys.Receivable: "Receivable", sys.Payable: "Payable", sys.TaxPayable: "Tax"}

	// Parents before children, whatever order the blueprint lists them in.
	pending := append([]domain.Account(nil), acct.ChartOfAccounts...)
	for len(pending) > 0 {
		progressed := false
		var next []domain.Account
		for _, a := range pending {
			parent := roots[rootType[a.Type]]
			if a.ParentCode != "" {
				p, ok := nameByCode[a.ParentCode]
				if !ok {
					next = append(next, a) // parent not created yet
					continue
				}
				parent = p
			}
			progressed = true
			if _, exists := nameByCode[a.Code]; exists {
				continue
			}
			body := map[string]any{
				"account_name":   a.Name,
				"account_number": a.Code,
				"company":        company,
				"parent_account": parent,
				"is_group":       boolInt(!a.Postable),
				"root_type":      rootType[a.Type],
			}
			if t, ok := accountType[a.Code]; ok && a.Postable {
				body["account_type"] = t
			}
			var created struct {
				Name string `json:"name"`
			}
			if err := c.do(ctx, http.MethodPost, "/api/resource/Account", nil, body, &created); err != nil {
				return fmt.Errorf("account %s %q: %w", a.Code, a.Name, err)
			}
			nameByCode[a.Code] = created.Name
		}
		if !progressed {
			return fmt.Errorf("erpnext: chart of accounts has accounts whose parents are missing: %v", codes(next))
		}
		pending = next
	}
	return nil
}

func codes(as []domain.Account) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Code
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ───────────────────────────── removal ─────────────────────────────

// RemoveCompany deletes the tenant's company if (and only if) it carries this tenant's marker. A missing company is
// success. ERPNext refuses to delete a company that has transactions; that error is returned as is.
func (c *Client) RemoveCompany(ctx context.Context, t ports.TenantInfo, ref ports.Ref) error {
	if err := c.validate(); err != nil {
		return err
	}
	if t.ID == uuid.Nil {
		return errors.New("erpnext: tenant id is required")
	}
	existing, err := c.findCompany(ctx, t.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	if ref != "" && string(ref) != existing.Name {
		return fmt.Errorf("erpnext: ref %q is not this tenant's company", ref)
	}
	err = c.do(ctx, http.MethodDelete, "/api/resource/Company/"+url.PathEscape(existing.Name), nil, nil, nil)
	if err != nil && !isStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}
