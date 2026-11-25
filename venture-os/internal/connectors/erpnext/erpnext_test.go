package erpnext_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/connectors/erpnext"
	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/ports"
)

var ctx = context.Background()

// fakeFrappe mimics the Frappe REST behaviours the adapter depends on: token auth, Host-routed site, the
// {"data": ...} envelope, JSON `filters`, 409 for duplicates, and the standard-template root accounts.
type fakeFrappe struct {
	mu         sync.Mutex
	companies  map[string]map[string]any
	accounts   map[string]map[string]any // by account name
	currencies map[string]int
	countries  map[string]string
	postedAcct int
	failAcctAt int // fail the Nth account POST with 500 (0 = never)
	hosts      []string
	srv        *httptest.Server
}

func newFrappe(t *testing.T) *fakeFrappe {
	t.Helper()
	f := &fakeFrappe{
		companies: map[string]map[string]any{}, accounts: map[string]map[string]any{},
		currencies: map[string]int{"INR": 0, "USD": 1}, countries: map[string]string{"in": "India", "us": "United States"},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFrappe) respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeFrappe) filters(r *http.Request) [][]any {
	var out [][]any
	_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &out)
	return out
}

func matches(row map[string]any, filters [][]any) bool {
	for _, f := range filters {
		field, op, want := f[0].(string), f[1].(string), f[2]
		got := row[field]
		switch op {
		case "=":
			if fmt.Sprint(got) != fmt.Sprint(want) && !(got == nil && want == "") {
				return false
			}
		case "!=":
			if fmt.Sprint(got) == fmt.Sprint(want) || (got == nil && want == "") {
				return false
			}
		case "like":
			if !strings.Contains(fmt.Sprint(got), strings.Trim(want.(string), "%")) {
				return false
			}
		}
	}
	return true
}

func (f *fakeFrappe) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts = append(f.hosts, r.Host)
	if r.Header.Get("Authorization") != "token key1:secret1" {
		f.respond(w, 401, map[string]any{"exc_type": "AuthenticationError", "message": "Invalid Authorization"})
		return
	}
	body := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/resource/")
	switch {
	case strings.HasPrefix(path, "Currency/"):
		code := strings.TrimPrefix(path, "Currency/")
		en, ok := f.currencies[code]
		if !ok {
			f.respond(w, 404, map[string]any{"exc_type": "DoesNotExistError"})
			return
		}
		if r.Method == http.MethodPut {
			f.currencies[code] = 1
			f.respond(w, 200, map[string]any{"data": map[string]any{"name": code, "enabled": 1}})
			return
		}
		f.respond(w, 200, map[string]any{"data": map[string]any{"name": code, "enabled": en}})
	case path == "Country":
		rows := []map[string]any{}
		for _, fl := range f.filters(r) {
			if name, ok := f.countries[fmt.Sprint(fl[2])]; ok {
				rows = append(rows, map[string]any{"name": name})
			}
		}
		f.respond(w, 200, map[string]any{"data": rows})
	case path == "Company" && r.Method == http.MethodGet:
		rows := []map[string]any{}
		for _, c := range f.companies {
			if matches(c, f.filters(r)) {
				rows = append(rows, c)
			}
		}
		f.respond(w, 200, map[string]any{"data": rows})
	case path == "Company" && r.Method == http.MethodPost:
		name := body["company_name"].(string)
		for _, c := range f.companies {
			if c["name"] == name || c["abbr"] == body["abbr"] {
				f.respond(w, 409, map[string]any{"exc_type": "DuplicateEntryError", "exception": "frappe.exceptions.DuplicateEntryError: Company " + name})
				return
			}
		}
		if f.currencies[body["default_currency"].(string)] != 1 {
			f.respond(w, 417, map[string]any{"exception": "frappe.exceptions.ValidationError: currency disabled"})
			return
		}
		body["name"] = name
		f.companies[name] = body
		abbr := body["abbr"].(string)
		for rt, n := range map[string]string{"Asset": "Application of Funds (Assets)", "Liability": "Source of Funds (Liabilities)", "Equity": "Equity", "Income": "Income", "Expense": "Expenses"} {
			an := n + " - " + abbr
			f.accounts[an] = map[string]any{"name": an, "company": name, "root_type": rt, "is_group": 1, "parent_account": ""}
		}
		f.respond(w, 200, map[string]any{"data": body})
	case strings.HasPrefix(path, "Company/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "Company/")
		if _, ok := f.companies[name]; !ok {
			f.respond(w, 404, map[string]any{"exc_type": "DoesNotExistError"})
			return
		}
		delete(f.companies, name)
		for k, a := range f.accounts {
			if a["company"] == name {
				delete(f.accounts, k)
			}
		}
		f.respond(w, 202, map[string]any{"message": "ok"})
	case path == "Account" && r.Method == http.MethodGet:
		rows := []map[string]any{}
		for _, a := range f.accounts {
			if matches(a, f.filters(r)) {
				rows = append(rows, a)
			}
		}
		f.respond(w, 200, map[string]any{"data": rows})
	case path == "Account" && r.Method == http.MethodPost:
		f.postedAcct++
		if f.failAcctAt != 0 && f.postedAcct == f.failAcctAt {
			f.respond(w, 500, map[string]any{"exception": "OperationalError: deadlock"})
			return
		}
		parent, ok := f.accounts[fmt.Sprint(body["parent_account"])]
		if !ok || parent["is_group"] != 1 && parent["is_group"] != float64(1) {
			f.respond(w, 417, map[string]any{"exception": "frappe.exceptions.ValidationError: parent is not a group"})
			return
		}
		if parent["root_type"] != body["root_type"] {
			f.respond(w, 417, map[string]any{"exception": "frappe.exceptions.ValidationError: root type mismatch"})
			return
		}
		abbr := f.companies[fmt.Sprint(body["company"])]["abbr"].(string)
		for _, a := range f.accounts {
			if a["company"] == body["company"] && a["account_number"] == body["account_number"] {
				f.respond(w, 409, map[string]any{"exc_type": "DuplicateEntryError"})
				return
			}
		}
		name := fmt.Sprintf("%s - %s - %s", body["account_number"], body["account_name"], abbr)
		body["name"] = name
		f.accounts[name] = body
		f.respond(w, 200, map[string]any{"data": body})
	default:
		f.respond(w, 404, map[string]any{"exc_type": "NotFound", "message": path})
	}
}

func (f *fakeFrappe) client() *erpnext.Client {
	return &erpnext.Client{BaseURL: f.srv.URL, Site: "erp.localhost", APIKey: "key1", APISecret: "secret1"}
}

func tenant(slug, name string) ports.TenantInfo {
	return ports.TenantInfo{ID: uuid.New(), Slug: slug, Name: name}
}

func (f *fakeFrappe) accountsOf(company string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, a := range f.accounts {
		if a["company"] == company && a["account_number"] != nil {
			out[fmt.Sprint(a["account_number"])] = a
		}
	}
	return out
}

func TestEnsureCompanyBuildsTheBlueprintChartUnderTheTemplateRoots(t *testing.T) {
	f := newFrappe(t)
	bp := fixtures.EXIMBlueprint()
	tn := tenant("kerala-spice-exports", bp.Business.Name)

	ref, err := f.client().EnsureCompany(ctx, tn, bp.Business, bp.Accounting)
	if err != nil {
		t.Fatal(err)
	}
	if string(ref) != "Kerala Spice Exports" {
		t.Fatalf("ref = %q", ref)
	}
	co := f.companies[string(ref)]
	if co["default_currency"] != "INR" || co["country"] != "India" || co["create_chart_of_accounts_based_on"] != "Standard Template" {
		t.Fatalf("company = %v", co)
	}
	if !strings.Contains(fmt.Sprint(co["company_description"]), "venture-tenant:"+tn.ID.String()) {
		t.Fatal("company is not marked with the tenant id")
	}
	if f.currencies["INR"] != 1 {
		t.Fatal("a disabled base currency was not enabled")
	}
	for _, h := range f.hosts {
		if h != "erp.localhost" {
			t.Fatalf("Host header = %q, frappe routes by it", h)
		}
	}

	accts := f.accountsOf(string(ref))
	if len(accts) != len(bp.Accounting.ChartOfAccounts) {
		t.Fatalf("created %d accounts, want %d", len(accts), len(bp.Accounting.ChartOfAccounts))
	}
	abbr := co["abbr"].(string)
	// Top-level accounts hang off the right template root; children off their blueprint group.
	if got := accts["1000"]["parent_account"]; got != "Application of Funds (Assets) - "+abbr {
		t.Errorf("1000 parent = %v", got)
	}
	if got := accts["3000"]["parent_account"]; got != "Equity - "+abbr {
		t.Errorf("3000 parent = %v", got)
	}
	if got := accts["4000"]["parent_account"]; got != "Income - "+abbr {
		t.Errorf("4000 parent = %v", got)
	}
	if got := accts["1010"]["parent_account"]; got != accts["1000"]["name"] {
		t.Errorf("1010 parent = %v, want %v", got, accts["1000"]["name"])
	}
	// Groups vs postable, and the ERPNext account types the finance engine relies on.
	if fmt.Sprint(accts["1000"]["is_group"]) != "1" || fmt.Sprint(accts["1010"]["is_group"]) != "0" {
		t.Errorf("group flags: 1000=%v 1010=%v", accts["1000"]["is_group"], accts["1010"]["is_group"])
	}
	for code, want := range map[string]string{"1010": "Bank", "1200": "Receivable", "2100": "Payable", "2200": "Tax"} {
		if accts[code]["account_type"] != want {
			t.Errorf("%s account_type = %v, want %s", code, accts[code]["account_type"], want)
		}
	}
	if _, has := accts["4000"]["account_type"]; has {
		t.Error("revenue account must not get an invented ERPNext account type")
	}
}

func TestEnsureIsIdempotentAndResumesAfterAPartialFailure(t *testing.T) {
	f := newFrappe(t)
	bp := fixtures.EXIMBlueprint()
	tn := tenant("kerala-spice-exports", bp.Business.Name)
	c := f.client()

	f.failAcctAt = 4 // the 4th account create blows up
	_, err := c.EnsureCompany(ctx, tn, bp.Business, bp.Accounting)
	if err == nil || !errors.Is(err, ports.ErrTransient) {
		t.Fatalf("partial failure should be a transient error, got %v", err)
	}
	partial := len(f.accountsOf("Kerala Spice Exports"))
	if partial != 3 {
		t.Fatalf("accounts after the failure = %d", partial)
	}
	f.failAcctAt = 0
	if _, err := c.EnsureCompany(ctx, tn, bp.Business, bp.Accounting); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(f.accountsOf("Kerala Spice Exports")); n != len(bp.Accounting.ChartOfAccounts) {
		t.Fatalf("accounts after resume = %d", n)
	}
	if len(f.companies) != 1 {
		t.Fatalf("companies = %d", len(f.companies))
	}
	posted := f.postedAcct
	if _, err := c.EnsureCompany(ctx, tn, bp.Business, bp.Accounting); err != nil {
		t.Fatal(err)
	}
	if f.postedAcct != posted {
		t.Fatalf("a third ensure created %d more accounts", f.postedAcct-posted)
	}
}

func TestParentsAreCreatedBeforeChildrenWhateverTheOrder(t *testing.T) {
	f := newFrappe(t)
	bp := fixtures.EXIMBlueprint()
	acc := bp.Accounting
	// Reverse the list so every child precedes its parent.
	for i, j := 0, len(acc.ChartOfAccounts)-1; i < j; i, j = i+1, j-1 {
		acc.ChartOfAccounts[i], acc.ChartOfAccounts[j] = acc.ChartOfAccounts[j], acc.ChartOfAccounts[i]
	}
	if _, err := f.client().EnsureCompany(ctx, tenant("kerala-spice-exports", "Kerala Spice Exports"), bp.Business, acc); err != nil {
		t.Fatal(err)
	}
	if n := len(f.accountsOf("Kerala Spice Exports")); n != len(acc.ChartOfAccounts) {
		t.Fatalf("accounts = %d", n)
	}
}

func TestNameCollisionWithAnotherCompanyIsDisambiguatedNotHijacked(t *testing.T) {
	f := newFrappe(t)
	f.companies["Kerala Spice Exports"] = map[string]any{"name": "Kerala Spice Exports", "abbr": "KSE", "company_description": "someone else's"}
	bp := fixtures.EXIMBlueprint()
	tn := tenant("kerala-spice-exports", bp.Business.Name)
	ref, err := f.client().EnsureCompany(ctx, tn, bp.Business, bp.Accounting)
	if err != nil {
		t.Fatal(err)
	}
	if string(ref) != "Kerala Spice Exports (kerala-spice-exports)" {
		t.Fatalf("ref = %q", ref)
	}
	if f.companies["Kerala Spice Exports"]["company_description"] != "someone else's" {
		t.Fatal("the pre-existing company was modified")
	}
	// A second ensure finds OUR company by its marker, not by the contested name.
	again, err := f.client().EnsureCompany(ctx, tn, bp.Business, bp.Accounting)
	if err != nil || again != ref {
		t.Fatalf("second ensure: %q %v", again, err)
	}
}

func TestTenantsGetSeparateCompanies(t *testing.T) {
	f := newFrappe(t)
	bp := fixtures.EXIMBlueprint()
	a, b := tenant("tenant-a", "Alpha Trading"), tenant("tenant-b", "Beta Trading")
	bpa, bpb := bp.Business, bp.Business
	bpa.Name, bpb.Name = "Alpha Trading", "Beta Trading"
	ra, err := f.client().EnsureCompany(ctx, a, bpa, bp.Accounting)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := f.client().EnsureCompany(ctx, b, bpb, bp.Accounting)
	if err != nil {
		t.Fatal(err)
	}
	if ra == rb || f.companies[string(ra)]["abbr"] == f.companies[string(rb)]["abbr"] {
		t.Fatal("tenants share a company or abbreviation")
	}
	if len(f.accountsOf(string(ra))) != len(f.accountsOf(string(rb))) || len(f.accountsOf(string(ra))) == 0 {
		t.Fatal("each tenant must get its own full chart")
	}
}

func TestRemoveOnlyDeletesTheTenantsOwnMarkedCompany(t *testing.T) {
	f := newFrappe(t)
	bp := fixtures.EXIMBlueprint()
	a, b := tenant("tenant-a", "Alpha Trading"), tenant("tenant-b", "Beta Trading")
	bpa, bpb := bp.Business, bp.Business
	bpa.Name, bpb.Name = "Alpha Trading", "Beta Trading"
	ra, _ := f.client().EnsureCompany(ctx, a, bpa, bp.Accounting)
	rb, _ := f.client().EnsureCompany(ctx, b, bpb, bp.Accounting)
	c := f.client()

	if err := c.RemoveCompany(ctx, a, rb); err == nil {
		t.Fatal("removed with another tenant's ref")
	}
	if _, ok := f.companies[string(rb)]; !ok {
		t.Fatal("tenant B's company was deleted")
	}
	if err := c.RemoveCompany(ctx, a, ra); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.companies[string(ra)]; ok {
		t.Fatal("company A still exists")
	}
	if n := len(f.accountsOf(string(ra))); n != 0 {
		t.Fatalf("company A's accounts remain: %d", n)
	}
	if err := c.RemoveCompany(ctx, a, ra); err != nil {
		t.Fatalf("second removal must succeed: %v", err)
	}
	// A company without our marker is never touched, even if a stale ref names it.
	f.companies["Manual Co"] = map[string]any{"name": "Manual Co", "abbr": "MC", "company_description": "hand made"}
	if err := c.RemoveCompany(ctx, tenant("unrelated", "x"), "Manual Co"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.companies["Manual Co"]; !ok {
		t.Fatal("an unmarked company was deleted")
	}
}

func TestErrorsAreClassifiedAndDoNotLeakCredentials(t *testing.T) {
	f := newFrappe(t)
	bp := fixtures.EXIMBlueprint()
	tn := tenant("kerala-spice-exports", bp.Business.Name)

	bad := f.client()
	bad.APISecret = "wrong"
	if _, err := bad.EnsureCompany(ctx, tn, bp.Business, bp.Accounting); err == nil || errors.Is(err, ports.ErrTransient) || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("auth failure: %v", err)
	}
	unknownCountry := bp.Business
	unknownCountry.Country = "ZZ"
	if _, err := f.client().EnsureCompany(ctx, tn, unknownCountry, bp.Accounting); err == nil || !strings.Contains(err.Error(), "country") {
		t.Fatalf("unknown country: %v", err)
	}
	unknownCurrency := bp.Business
	unknownCurrency.BaseCurrency = "XXX"
	if _, err := f.client().EnsureCompany(ctx, tn, unknownCurrency, bp.Accounting); err == nil || !strings.Contains(err.Error(), "currency") {
		t.Fatalf("unknown currency: %v", err)
	}
	dead := &erpnext.Client{BaseURL: "http://127.0.0.1:1", Site: "x", APIKey: "k", APISecret: "s"}
	if _, err := dead.EnsureCompany(ctx, tn, bp.Business, bp.Accounting); !errors.Is(err, ports.ErrTransient) {
		t.Fatalf("unreachable: %v", err)
	}
	if _, err := (&erpnext.Client{}).EnsureCompany(ctx, tn, bp.Business, bp.Accounting); err == nil {
		t.Fatal("empty config accepted")
	}
	if _, err := f.client().EnsureCompany(ctx, ports.TenantInfo{Slug: "x"}, bp.Business, bp.Accounting); err == nil {
		t.Fatal("nil tenant id accepted")
	}
	_ = domain.AccountAsset
}
