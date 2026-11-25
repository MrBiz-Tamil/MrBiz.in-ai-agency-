// Package crmtest is the contract every crm.CRM backend must satisfy. A backend's own test file calls Run with a
// factory; the built-in Postgres backend and the in-memory reference both run it, and so must every future adapter
// (Twenty, HubSpot, Salesforce, Zoho, Odoo). A backend is trusted only when this suite passes.
//
// The suite is deliberately about observable behaviour: validation, idempotency, ordering and paging, search
// semantics, optimistic concurrency, archive semantics and tenant isolation. It says nothing about storage.
package crmtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/domain"
)

// Backend is what a backend's test file supplies.
type Backend interface {
	crm.Provider
	// NewTenant creates a tenant whose CRM sees exactly these pipelines (in production, provisioning does this).
	NewTenant(t *testing.T, pipelines []crm.Pipeline) uuid.UUID
}

// Pipelines is the fixture used by the suite: a sales pipeline with typed custom fields and a second pipeline.
func Pipelines() []crm.Pipeline {
	return []crm.Pipeline{
		{
			Key: "sales", Name: "Export sales", EntityType: domain.EntityDeal,
			Stages: []domain.Stage{
				{Key: "lead", Name: "Lead", WinProbability: 1000},
				{Key: "qualified", Name: "Qualified", WinProbability: 3000},
				{Key: "quoted", Name: "Quoted", WinProbability: 5000},
				{Key: "won", Name: "Won", WinProbability: 10000},
				{Key: "lost", Name: "Lost", WinProbability: 0},
			},
			CustomFields: []domain.CustomField{
				{Key: "incoterm", Label: "Incoterm", Type: "SELECT", Required: true},
				{Key: "destination", Label: "Destination", Type: "TEXT"},
				{Key: "volume_kg", Label: "Volume (kg)", Type: "NUMBER"},
				{Key: "start_on", Label: "Start date", Type: "DATE"},
				{Key: "sample_sent", Label: "Sample sent", Type: "BOOLEAN"},
			},
		},
		{
			Key: "suppliers", Name: "Suppliers", EntityType: domain.EntityLead,
			Stages: []domain.Stage{{Key: "identified", Name: "Identified"}, {Key: "active", Name: "Active"}},
		},
	}
}

var ctx = context.Background()

// Run executes the contract against a backend built once by newBackend. Every subtest uses fresh tenants, so
// subtests never see each other's data.
func Run(t *testing.T, newBackend func(t *testing.T) Backend) {
	t.Helper()
	b := newBackend(t)
	for _, c := range []struct {
		name string
		fn   func(t *testing.T, b Backend)
	}{
		{"Pipelines", testPipelines},
		{"CompanyLifecycle", testCompanyLifecycle},
		{"Idempotency", testIdempotency},
		{"ValidationCreatesNothing", testValidation},
		{"Contacts", testContacts},
		{"Deals", testDeals},
		{"MoveDealAndHistory", testMoveDeal},
		{"OptimisticConcurrency", testConcurrency},
		{"Activities", testActivities},
		{"Pagination", testPagination},
		{"SearchIsLiteralAndCaseInsensitive", testSearch},
		{"MalformedIDsBehaveLikeUnknownIDs", testMalformedIDs},
		{"TenantIsolation", testIsolation},
		{"CancelledContext", testCancelled},
		{"UnicodeAndLimits", testUnicode},
		{"ResultsAreCopies", testCopies},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, b) })
	}
}

// ───────────────────────────── helpers ─────────────────────────────

func tenant(t *testing.T, b Backend) crm.CRM {
	t.Helper()
	c, err := b.For(ctx, b.NewTenant(t, Pipelines()))
	must(t, err)
	return c
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func isErr(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: want %v, got %v", what, want, err)
	}
}

func key() string { return uuid.NewString() }

func newCompany(t *testing.T, c crm.CRM, name string) crm.Company {
	t.Helper()
	co, created, err := c.CreateCompany(ctx, key(), crm.CompanyInput{Name: name})
	must(t, err)
	if !created {
		t.Fatal("a fresh key must create")
	}
	return co
}

func dealInput(company crm.ID) crm.DealInput {
	return crm.DealInput{Name: "Pepper 1000 kg", CompanyID: company, Pipeline: "sales", Stage: "lead", CreatedBy: "creator",
		Fields: map[string]string{"incoterm": "FOB"}}
}

func newDeal(t *testing.T, c crm.CRM, company crm.ID) crm.Deal {
	t.Helper()
	d, _, err := c.CreateDeal(ctx, key(), dealInput(company))
	must(t, err)
	return d
}

func counts(t *testing.T, c crm.CRM) (companies, contacts, deals, activities int) {
	t.Helper()
	all := crm.PageRequest{Size: crm.MaxPageSize}
	a, err := c.ListCompanies(ctx, crm.CompanyFilter{IncludeArchived: true}, all)
	must(t, err)
	b, err := c.ListContacts(ctx, crm.ContactFilter{IncludeArchived: true}, all)
	must(t, err)
	d, err := c.ListDeals(ctx, crm.DealFilter{IncludeArchived: true}, all)
	must(t, err)
	e, err := c.ListActivities(ctx, crm.ActivityFilter{}, all)
	must(t, err)
	return len(a.Items), len(b.Items), len(d.Items), len(e.Items)
}

func ptr[T any](v T) *T { return &v }

// ───────────────────────────── pipelines ─────────────────────────────

func testPipelines(t *testing.T, b Backend) {
	c := tenant(t, b)
	ps, err := c.Pipelines(ctx)
	must(t, err)
	if len(ps) != 2 || ps[0].Key != "sales" || ps[1].Key != "suppliers" {
		t.Fatalf("pipelines = %+v", ps)
	}
	var stages []string
	for _, s := range ps[0].Stages {
		stages = append(stages, s.Key)
	}
	if strings.Join(stages, ",") != "lead,qualified,quoted,won,lost" {
		t.Fatalf("stage order not preserved: %v", stages)
	}
	if len(ps[0].CustomFields) != 5 {
		t.Fatalf("custom fields = %d", len(ps[0].CustomFields))
	}

	// A tenant sees only its own pipelines, and cannot use another's.
	other, err := b.For(ctx, b.NewTenant(t, Pipelines()[1:]))
	must(t, err)
	ops, _ := other.Pipelines(ctx)
	if len(ops) != 1 || ops[0].Key != "suppliers" {
		t.Fatalf("other tenant's pipelines = %+v", ops)
	}
	co := newCompany(t, other, "Other Co")
	_, _, err = other.CreateDeal(ctx, key(), dealInput(co.ID))
	isErr(t, err, crm.ErrInvalid, "deal on a pipeline the tenant does not have")
}

// ───────────────────────────── companies ─────────────────────────────

func testCompanyLifecycle(t *testing.T, b Backend) {
	c := tenant(t, b)
	caps := c.Capabilities()
	co, created, err := c.CreateCompany(ctx, key(), crm.CompanyInput{
		Name: "  Acme Spices GmbH ", Domain: " ACME-Spices.DE ", Country: "de", Industry: "food",
		OwnerSub: "rep-1", Fields: map[string]string{"tier": "gold", "region": "emea"},
	})
	must(t, err)
	if !created || co.ID == "" {
		t.Fatalf("create: %+v created=%v", co, created)
	}
	if co.Name != "Acme Spices GmbH" || co.Domain != "acme-spices.de" || co.Country != "DE" || co.OwnerSub != "rep-1" {
		t.Fatalf("input was not normalized: %+v", co)
	}
	if co.CreatedAt.IsZero() || co.UpdatedAt.Before(co.CreatedAt) {
		t.Fatalf("timestamps: %v %v", co.CreatedAt, co.UpdatedAt)
	}
	if caps.OptimisticConcurrency && co.Version != 1 {
		t.Fatalf("version = %d, want 1", co.Version)
	}
	got, err := c.GetCompany(ctx, co.ID)
	must(t, err)
	if got.ID != co.ID || got.Name != co.Name || got.Fields["tier"] != "gold" {
		t.Fatalf("get = %+v", got)
	}

	// Patch changes only what is set; fields merge; an empty value removes a key.
	up, err := c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Name: ptr("Acme Ltd"), Fields: map[string]string{"tier": "", "segment": "wholesale"}}, 0)
	must(t, err)
	if up.Name != "Acme Ltd" || up.Domain != "acme-spices.de" || up.Country != "DE" || up.OwnerSub != "rep-1" {
		t.Fatalf("untouched fields changed: %+v", up)
	}
	if _, has := up.Fields["tier"]; has || up.Fields["region"] != "emea" || up.Fields["segment"] != "wholesale" {
		t.Fatalf("field merge wrong: %v", up.Fields)
	}
	if up.UpdatedAt.Before(co.UpdatedAt) {
		t.Fatal("updated_at went backwards")
	}
	if caps.OptimisticConcurrency && up.Version != 2 {
		t.Fatalf("version after update = %d", up.Version)
	}
	// An invalid patch changes nothing.
	_, err = c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Country: ptr("Germany")}, 0)
	isErr(t, err, crm.ErrInvalid, "invalid patch")
	if again, _ := c.GetCompany(ctx, co.ID); again.Name != "Acme Ltd" {
		t.Fatal("invalid patch partially applied")
	}

	// Archive hides the record everywhere except IncludeArchived lists.
	must(t, c.ArchiveCompany(ctx, co.ID, 0))
	_, err = c.GetCompany(ctx, co.ID)
	isErr(t, err, crm.ErrNotFound, "get archived")
	_, err = c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Name: ptr("x")}, 0)
	isErr(t, err, crm.ErrNotFound, "update archived")
	isErr(t, c.ArchiveCompany(ctx, co.ID, 0), crm.ErrNotFound, "archive twice")
	live, _ := c.ListCompanies(ctx, crm.CompanyFilter{}, crm.PageRequest{})
	if len(live.Items) != 0 {
		t.Fatalf("archived company listed: %+v", live.Items)
	}
	all, _ := c.ListCompanies(ctx, crm.CompanyFilter{IncludeArchived: true}, crm.PageRequest{})
	if len(all.Items) != 1 || all.Items[0].ID != co.ID {
		t.Fatalf("IncludeArchived: %+v", all.Items)
	}
}

// ───────────────────────────── idempotency ─────────────────────────────

func testIdempotency(t *testing.T, b Backend) {
	c := tenant(t, b)
	parent := newCompany(t, c, "Parent")
	type creator func(key string, variant bool) (crm.ID, bool, error)
	kinds := map[string]creator{
		"company": func(k string, v bool) (crm.ID, bool, error) {
			name := "Idem"
			if v {
				name = "Idem changed"
			}
			x, created, err := c.CreateCompany(ctx, k, crm.CompanyInput{Name: name})
			return x.ID, created, err
		},
		"contact": func(k string, v bool) (crm.ID, bool, error) {
			email := "a@example.com"
			if v {
				email = "b@example.com"
			}
			x, created, err := c.CreateContact(ctx, k, crm.ContactInput{Email: email, CompanyID: parent.ID})
			return x.ID, created, err
		},
		"deal": func(k string, v bool) (crm.ID, bool, error) {
			in := dealInput(parent.ID)
			if v {
				in.Name = "Changed"
			}
			x, created, err := c.CreateDeal(ctx, k, in)
			return x.ID, created, err
		},
		"activity": func(k string, v bool) (crm.ID, bool, error) {
			subj := "Call"
			if v {
				subj = "Call changed"
			}
			x, created, err := c.CreateActivity(ctx, k, crm.ActivityInput{Kind: crm.ActivityCall, Subject: subj, CompanyID: parent.ID})
			return x.ID, created, err
		},
	}
	shared := key() // the SAME key string is used for every kind: keys are scoped per kind
	for name, create := range kinds {
		id1, created, err := create(shared, false)
		must(t, err)
		if !created || id1 == "" {
			t.Fatalf("%s: first create: id=%q created=%v", name, id1, created)
		}
		id2, created, err := create(shared, false)
		must(t, err)
		if created || id2 != id1 {
			t.Fatalf("%s: replay must return the original (id %q vs %q, created=%v)", name, id2, id1, created)
		}
		_, _, err = create(shared, true)
		isErr(t, err, crm.ErrIdempotencyMismatch, name+": key reused with different input")
		_, _, err = create("", false)
		isErr(t, err, crm.ErrInvalid, name+": empty key")
		id3, created, err := create(key(), false)
		must(t, err)
		if !created || id3 == id1 {
			t.Fatalf("%s: a new key must create a new record", name)
		}
	}
	// Replaying never duplicates.
	if co, _, de, ac := counts(t, c); co != 3 || de != 2 || ac != 2 {
		t.Fatalf("counts after replays: companies=%d deals=%d activities=%d", co, de, ac)
	}
}

// ───────────────────────────── validation ─────────────────────────────

func testValidation(t *testing.T, b Backend) {
	c := tenant(t, b)
	co := newCompany(t, c, "Base")
	before1, before2, before3, before4 := counts(t, c)
	long := strings.Repeat("x", crm.MaxNameLen+1)
	bad := map[string]func() error{
		"company: empty name": func() error { _, _, e := c.CreateCompany(ctx, key(), crm.CompanyInput{}); return e },
		"company: long name":  func() error { _, _, e := c.CreateCompany(ctx, key(), crm.CompanyInput{Name: long}); return e },
		"company: bad country": func() error {
			_, _, e := c.CreateCompany(ctx, key(), crm.CompanyInput{Name: "a", Country: "DEU"})
			return e
		},
		"company: bad domain": func() error {
			_, _, e := c.CreateCompany(ctx, key(), crm.CompanyInput{Name: "a", Domain: "no spaces"})
			return e
		},
		"company: bad field key": func() error {
			_, _, e := c.CreateCompany(ctx, key(), crm.CompanyInput{Name: "a", Fields: map[string]string{"Bad": "x"}})
			return e
		},
		"contact: nothing":   func() error { _, _, e := c.CreateContact(ctx, key(), crm.ContactInput{}); return e },
		"contact: bad email": func() error { _, _, e := c.CreateContact(ctx, key(), crm.ContactInput{Email: "nope"}); return e },
		"contact: unknown company": func() error {
			_, _, e := c.CreateContact(ctx, key(), crm.ContactInput{Email: "a@b.co", CompanyID: crm.ID(uuid.NewString())})
			return e
		},
		"deal: no company":      func() error { in := dealInput(""); _, _, e := c.CreateDeal(ctx, key(), in); return e },
		"deal: unknown company": func() error { _, _, e := c.CreateDeal(ctx, key(), dealInput(crm.ID(uuid.NewString()))); return e },
		"deal: unknown stage": func() error {
			in := dealInput(co.ID)
			in.Stage = "shipped"
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"deal: unknown pipeline": func() error {
			in := dealInput(co.ID)
			in.Pipeline = "nope"
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"deal: missing required field": func() error {
			in := dealInput(co.ID)
			in.Fields = nil
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"deal: undefined field": func() error {
			in := dealInput(co.ID)
			in.Fields["hack"] = "1"
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"deal: NUMBER field": func() error {
			in := dealInput(co.ID)
			in.Fields["volume_kg"] = "lots"
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"deal: DATE field": func() error {
			in := dealInput(co.ID)
			in.Fields["start_on"] = "1/2/2026"
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"deal: BOOLEAN field": func() error {
			in := dealInput(co.ID)
			in.Fields["sample_sent"] = "yes"
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"deal: negative amount": func() error {
			in := dealInput(co.ID)
			in.Amount = &crm.Money{Minor: -1, Currency: "USD"}
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"deal: bad currency": func() error {
			in := dealInput(co.ID)
			in.Amount = &crm.Money{Minor: 1, Currency: "dollars"}
			_, _, e := c.CreateDeal(ctx, key(), in)
			return e
		},
		"activity: bad kind": func() error {
			_, _, e := c.CreateActivity(ctx, key(), crm.ActivityInput{Kind: "SMOKE", Subject: "s", CompanyID: co.ID})
			return e
		},
		"activity: no parent": func() error {
			_, _, e := c.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityNote, Subject: "s"})
			return e
		},
		"activity: note with due date": func() error {
			_, _, e := c.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityNote, Subject: "s", CompanyID: co.ID, DueAt: time.Now()})
			return e
		},
		"activity: unknown parent": func() error {
			_, _, e := c.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityNote, Subject: "s", DealID: crm.ID(uuid.NewString())})
			return e
		},
	}
	for name, f := range bad {
		isErr(t, f(), crm.ErrInvalid, name)
	}
	a, bb, cc, dd := counts(t, c)
	if a != before1 || bb != before2 || cc != before3 || dd != before4 {
		t.Fatalf("a rejected create left records behind: %d/%d/%d/%d -> %d/%d/%d/%d", before1, before2, before3, before4, a, bb, cc, dd)
	}
	_, err := c.ListCompanies(ctx, crm.CompanyFilter{}, crm.PageRequest{Size: crm.MaxPageSize + 1})
	isErr(t, err, crm.ErrInvalid, "page size over the maximum")
}

// ───────────────────────────── contacts ─────────────────────────────

func testContacts(t *testing.T, b Backend) {
	c := tenant(t, b)
	co := newCompany(t, c, "Acme")
	other := newCompany(t, c, "Other")
	k1, _, err := c.CreateContact(ctx, key(), crm.ContactInput{CompanyID: co.ID, FirstName: "Ada", LastName: "Lovelace", Email: " ADA@Acme.COM ", Title: "Buyer", OwnerSub: "rep-1"})
	must(t, err)
	if k1.Email != "ada@acme.com" || k1.CompanyID != co.ID || k1.FirstName != "Ada" {
		t.Fatalf("contact = %+v", k1)
	}
	_, _, err = c.CreateContact(ctx, key(), crm.ContactInput{CompanyID: other.ID, Email: "bob@other.com"})
	must(t, err)
	_, _, err = c.CreateContact(ctx, key(), crm.ContactInput{FirstName: "Loner"}) // a contact need not have a company
	must(t, err)

	byCompany, err := c.ListContacts(ctx, crm.ContactFilter{CompanyID: co.ID}, crm.PageRequest{})
	must(t, err)
	if len(byCompany.Items) != 1 || byCompany.Items[0].ID != k1.ID {
		t.Fatalf("company filter: %+v", byCompany.Items)
	}
	byEmail, _ := c.ListContacts(ctx, crm.ContactFilter{Query: "ACME.com"}, crm.PageRequest{})
	if len(byEmail.Items) != 1 {
		t.Fatalf("email query: %+v", byEmail.Items)
	}
	byOwner, _ := c.ListContacts(ctx, crm.ContactFilter{OwnerSub: "rep-1"}, crm.PageRequest{})
	if len(byOwner.Items) != 1 {
		t.Fatalf("owner filter: %+v", byOwner.Items)
	}

	up, err := c.UpdateContact(ctx, k1.ID, crm.ContactPatch{Title: ptr("Head Buyer"), Email: ptr("ADA@NEW.com")}, 0)
	must(t, err)
	if up.Title != "Head Buyer" || up.Email != "ada@new.com" || up.LastName != "Lovelace" || up.CompanyID != co.ID {
		t.Fatalf("patch = %+v", up)
	}
	_, err = c.UpdateContact(ctx, k1.ID, crm.ContactPatch{Email: ptr("broken")}, 0)
	isErr(t, err, crm.ErrInvalid, "bad email patch")

	// A contact cannot be attached to an archived company.
	must(t, c.ArchiveCompany(ctx, other.ID, 0))
	_, _, err = c.CreateContact(ctx, key(), crm.ContactInput{CompanyID: other.ID, Email: "late@other.com"})
	isErr(t, err, crm.ErrInvalid, "contact on an archived company")

	must(t, c.ArchiveContact(ctx, k1.ID, 0))
	_, err = c.GetContact(ctx, k1.ID)
	isErr(t, err, crm.ErrNotFound, "get archived contact")
}

// ───────────────────────────── deals ─────────────────────────────

func testDeals(t *testing.T, b Backend) {
	c := tenant(t, b)
	co := newCompany(t, c, "Acme")
	contact, _, err := c.CreateContact(ctx, key(), crm.ContactInput{CompanyID: co.ID, Email: "ada@acme.com"})
	must(t, err)

	in := dealInput(co.ID)
	in.ContactID = contact.ID
	in.OwnerSub = "rep-1"
	in.Amount = &crm.Money{Minor: 15000000, Currency: "usd"}
	in.ExpectedClose = time.Date(2026, 3, 15, 22, 0, 0, 0, time.UTC)
	in.Fields = map[string]string{"incoterm": "CIF", "volume_kg": "1500.5", "start_on": "2026-04-01", "sample_sent": "true", "destination": "Hamburg"}
	d, created, err := c.CreateDeal(ctx, key(), in)
	must(t, err)
	if !created || d.Stage != "lead" || d.Pipeline != "sales" || d.CompanyID != co.ID || d.ContactID != contact.ID {
		t.Fatalf("deal = %+v", d)
	}
	if d.Amount == nil || d.Amount.Minor != 15000000 || d.Amount.Currency != "USD" {
		t.Fatalf("amount = %+v", d.Amount)
	}
	if !d.ExpectedClose.Equal(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected close must be a UTC date, got %v", d.ExpectedClose)
	}
	if d.StageChangedAt.IsZero() || d.Fields["destination"] != "Hamburg" {
		t.Fatalf("deal = %+v", d)
	}

	// Update: name, amount cleared then set, date cleared, fields merged, contact changed.
	up, err := c.UpdateDeal(ctx, d.ID, crm.DealPatch{Name: ptr("Pepper 2000 kg"), ClearAmount: true, ClearExpectedClose: true, Fields: map[string]string{"destination": "", "volume_kg": "2000"}}, 0)
	must(t, err)
	if up.Name != "Pepper 2000 kg" || up.Amount != nil || !up.ExpectedClose.IsZero() || up.Fields["volume_kg"] != "2000" || up.Fields["incoterm"] != "CIF" {
		t.Fatalf("patched deal = %+v", up)
	}
	if _, has := up.Fields["destination"]; has {
		t.Fatal("optional field was not removed")
	}
	if up.Stage != "lead" || up.CompanyID != co.ID {
		t.Fatal("a patch must not change stage or company")
	}
	up, err = c.UpdateDeal(ctx, d.ID, crm.DealPatch{Amount: &crm.Money{Minor: 99, Currency: "EUR"}}, 0)
	must(t, err)
	if up.Amount == nil || up.Amount.Minor != 99 || up.Amount.Currency != "EUR" {
		t.Fatalf("amount patch = %+v", up.Amount)
	}
	for name, p := range map[string]crm.DealPatch{
		"remove required field": {Fields: map[string]string{"incoterm": ""}},
		"undefined field":       {Fields: map[string]string{"hack": "1"}},
		"bad number":            {Fields: map[string]string{"volume_kg": "x"}},
		"unknown contact":       {ContactID: ptr(crm.ID(uuid.NewString()))},
		"set and clear amount":  {Amount: &crm.Money{Minor: 1, Currency: "USD"}, ClearAmount: true},
	} {
		_, err := c.UpdateDeal(ctx, d.ID, p, 0)
		isErr(t, err, crm.ErrInvalid, "patch: "+name)
	}

	// Filters.
	d2in := dealInput(co.ID)
	d2in.Name = "Cardamom"
	d2in.Stage = "quoted"
	d2in.OwnerSub = "rep-2"
	d2, _, err := c.CreateDeal(ctx, key(), d2in)
	must(t, err)
	supp, _, err := c.CreateDeal(ctx, key(), crm.DealInput{Name: "Farm co-op", CompanyID: co.ID, Pipeline: "suppliers", Stage: "identified"})
	must(t, err)
	list := func(f crm.DealFilter) []crm.Deal {
		p, err := c.ListDeals(ctx, f, crm.PageRequest{})
		must(t, err)
		return p.Items
	}
	if got := list(crm.DealFilter{}); len(got) != 3 {
		t.Fatalf("all deals = %d", len(got))
	}
	if got := list(crm.DealFilter{Pipeline: "suppliers"}); len(got) != 1 || got[0].ID != supp.ID {
		t.Fatalf("pipeline filter: %+v", got)
	}
	if got := list(crm.DealFilter{Pipeline: "sales", Stage: "quoted"}); len(got) != 1 || got[0].ID != d2.ID {
		t.Fatalf("stage filter: %+v", got)
	}
	if got := list(crm.DealFilter{OwnerSub: "rep-1"}); len(got) != 1 || got[0].ID != d.ID {
		t.Fatalf("owner filter: %+v", got)
	}
	if got := list(crm.DealFilter{Query: "CARDAM"}); len(got) != 1 || got[0].ID != d2.ID {
		t.Fatalf("query filter: %+v", got)
	}
	if got := list(crm.DealFilter{CompanyID: co.ID}); len(got) != 3 {
		t.Fatalf("company filter: %d", len(got))
	}

	must(t, c.ArchiveDeal(ctx, d2.ID, 0))
	_, err = c.GetDeal(ctx, d2.ID)
	isErr(t, err, crm.ErrNotFound, "get archived deal")
	_, err = c.MoveDeal(ctx, d2.ID, "won", "rep", 0)
	isErr(t, err, crm.ErrNotFound, "move archived deal")
	if got := list(crm.DealFilter{}); len(got) != 2 {
		t.Fatalf("archived deal still listed: %d", len(got))
	}
}

func testMoveDeal(t *testing.T, b Backend) {
	c := tenant(t, b)
	caps := c.Capabilities()
	d := newDeal(t, c, newCompany(t, c, "Acme").ID)

	m1, err := c.MoveDeal(ctx, d.ID, "qualified", "rep1", 0)
	must(t, err)
	if m1.Stage != "qualified" || m1.StageChangedAt.Before(d.StageChangedAt) {
		t.Fatalf("move: %+v", m1)
	}
	if caps.OptimisticConcurrency && m1.Version != d.Version+1 {
		t.Fatalf("version %d -> %d", d.Version, m1.Version)
	}
	// Moving to the current stage is a no-op: nothing changes and no history entry appears.
	same, err := c.MoveDeal(ctx, d.ID, "qualified", "rep1", 0)
	must(t, err)
	if same.Version != m1.Version || !same.StageChangedAt.Equal(m1.StageChangedAt) {
		t.Fatalf("no-op move changed the deal: %+v vs %+v", same, m1)
	}
	if _, err := c.MoveDeal(ctx, d.ID, "quoted", "rep1", 0); err != nil {
		t.Fatal(err)
	}
	back, err := c.MoveDeal(ctx, d.ID, "qualified", "rep2", 0) // stages are data: moving back is allowed here
	must(t, err)
	if back.Stage != "qualified" {
		t.Fatalf("backward move: %+v", back)
	}
	_, err = c.MoveDeal(ctx, d.ID, "shipped", "rep1", 0)
	isErr(t, err, crm.ErrInvalid, "unknown stage")
	if g, _ := c.GetDeal(ctx, d.ID); g.Stage != "qualified" {
		t.Fatal("a rejected move changed the stage")
	}
	_, err = c.MoveDeal(ctx, "no-such-deal", "won", "x", 0)
	isErr(t, err, crm.ErrNotFound, "move unknown deal")

	h, err := c.DealHistory(ctx, d.ID)
	if !caps.StageHistory {
		isErr(t, err, crm.ErrUnsupported, "history without the capability")
		return
	}
	must(t, err)
	var path []string
	for _, e := range h {
		path = append(path, e.From+">"+e.To)
	}
	want := ">lead,lead>qualified,qualified>quoted,quoted>qualified"
	if strings.Join(path, ",") != want {
		t.Fatalf("history = %v, want %s", path, want)
	}
	if h[0].ChangedBy != "creator" {
		t.Fatalf("the first history entry must name who created the deal: %+v", h[0])
	}
	if h[1].ChangedBy != "rep1" || h[3].ChangedBy != "rep2" || h[1].ChangedAt.IsZero() {
		t.Fatalf("history details: %+v", h)
	}
}

// ───────────────────────────── optimistic concurrency ─────────────────────────────

func testConcurrency(t *testing.T, b Backend) {
	c := tenant(t, b)
	co := newCompany(t, c, "Acme")
	if !c.Capabilities().OptimisticConcurrency {
		// A backend that cannot check versions must say so, never silently ignore the check.
		_, err := c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Name: ptr("x")}, 1)
		isErr(t, err, crm.ErrUnsupported, "ifVersion without the capability")
		return
	}
	d := newDeal(t, c, co.ID)

	_, err := c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Name: ptr("stale")}, co.Version+7)
	isErr(t, err, crm.ErrConflict, "stale company update")
	if g, _ := c.GetCompany(ctx, co.ID); g.Name != "Acme" || g.Version != co.Version {
		t.Fatalf("a conflicting update changed the record: %+v", g)
	}
	_, err = c.MoveDeal(ctx, d.ID, "won", "x", d.Version+3)
	isErr(t, err, crm.ErrConflict, "stale move")
	_, err = c.UpdateDeal(ctx, d.ID, crm.DealPatch{Name: ptr("stale")}, d.Version+3)
	isErr(t, err, crm.ErrConflict, "stale deal update")
	isErr(t, c.ArchiveCompany(ctx, co.ID, co.Version+3), crm.ErrConflict, "stale archive")
	if _, err := c.GetCompany(ctx, co.ID); err != nil {
		t.Fatal("a conflicting archive removed the record")
	}

	up, err := c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Name: ptr("fresh")}, co.Version)
	must(t, err)
	if up.Version != co.Version+1 {
		t.Fatalf("version = %d", up.Version)
	}
	// ifVersion 0 means "do not check".
	if _, err := c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Name: ptr("unchecked")}, 0); err != nil {
		t.Fatal(err)
	}

	// Writers racing from the same version: exactly one wins each round. All of them are released at once, over
	// many rounds, because a single quick race on a backend with a warm connection pool often serialises by luck.
	for round := 0; round < 25; round++ {
		base, _ := c.GetCompany(ctx, co.ID)
		var wins, conflicts int
		var mu sync.Mutex
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-gate
				_, err := c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Name: ptr(fmt.Sprintf("racer-%d-%d", round, i))}, base.Version)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case errors.Is(err, crm.ErrConflict):
					conflicts++
				default:
					t.Errorf("round %d racer %d: %v", round, i, err)
				}
			}(i)
		}
		close(gate)
		wg.Wait()
		if wins != 1 || conflicts != 7 {
			t.Fatalf("round %d: wins=%d conflicts=%d, want 1 and 7", round, wins, conflicts)
		}
		if g, _ := c.GetCompany(ctx, co.ID); g.Version != base.Version+1 {
			t.Fatalf("round %d: version after the race = %d, want %d", round, g.Version, base.Version+1)
		}
	}
}

// ───────────────────────────── activities ─────────────────────────────

func testActivities(t *testing.T, b Backend) {
	c := tenant(t, b)
	co := newCompany(t, c, "Acme")
	d := newDeal(t, c, co.ID)

	note, created, err := c.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityNote, Subject: " Sample feedback ", Body: "Loved it", DealID: d.ID, CreatedBy: "rep-1", OwnerSub: "rep-1"})
	must(t, err)
	if !created || note.ID == "" || note.Subject != "Sample feedback" || note.DealID != d.ID || note.CreatedAt.IsZero() {
		t.Fatalf("note = %+v", note)
	}
	due := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	task, _, err := c.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityTask, Subject: "Send quote", DealID: d.ID, CompanyID: co.ID, DueAt: due, OwnerSub: "rep-2", CreatedBy: "rep-1"})
	must(t, err)
	if !task.DueAt.Equal(due) || !task.CompletedAt.IsZero() {
		t.Fatalf("task = %+v", task)
	}
	call, _, err := c.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityCall, Subject: "Intro call", CompanyID: co.ID})
	must(t, err)

	got, err := c.GetActivity(ctx, note.ID)
	must(t, err)
	if got.ID != note.ID || got.Body != "Loved it" {
		t.Fatalf("get = %+v", got)
	}

	// Completing: only tasks; completing twice returns the same completion.
	_, err = c.CompleteActivity(ctx, note.ID, "rep-1")
	isErr(t, err, crm.ErrInvalid, "completing a note")
	done, err := c.CompleteActivity(ctx, task.ID, "rep-2")
	must(t, err)
	if done.CompletedAt.IsZero() {
		t.Fatal("task not completed")
	}
	again, err := c.CompleteActivity(ctx, task.ID, "rep-3")
	must(t, err)
	if !again.CompletedAt.Equal(done.CompletedAt) {
		t.Fatal("completing twice must not change the completion time")
	}
	_, err = c.CompleteActivity(ctx, "no-such-activity", "x")
	isErr(t, err, crm.ErrNotFound, "complete unknown activity")

	list := func(f crm.ActivityFilter) []crm.Activity {
		p, err := c.ListActivities(ctx, f, crm.PageRequest{})
		must(t, err)
		return p.Items
	}
	if got := list(crm.ActivityFilter{DealID: d.ID}); len(got) != 2 || got[0].ID != note.ID || got[1].ID != task.ID {
		t.Fatalf("deal activities (oldest first): %+v", got)
	}
	if got := list(crm.ActivityFilter{CompanyID: co.ID}); len(got) != 2 {
		t.Fatalf("company activities: %d", len(got))
	}
	if got := list(crm.ActivityFilter{Kind: crm.ActivityCall}); len(got) != 1 || got[0].ID != call.ID {
		t.Fatalf("kind filter: %+v", got)
	}
	if got := list(crm.ActivityFilter{OwnerSub: "rep-2"}); len(got) != 1 || got[0].ID != task.ID {
		t.Fatalf("owner filter: %+v", got)
	}
	if got := list(crm.ActivityFilter{OpenTasksOnly: true}); len(got) != 0 {
		t.Fatalf("open tasks after completion: %+v", got)
	}
	open, _, err := c.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityTask, Subject: "Follow up", ContactID: "", CompanyID: co.ID})
	must(t, err)
	if got := list(crm.ActivityFilter{OpenTasksOnly: true}); len(got) != 1 || got[0].ID != open.ID {
		t.Fatalf("open tasks: %+v", got)
	}
}

// ───────────────────────────── pagination ─────────────────────────────

func testPagination(t *testing.T, b Backend) {
	c := tenant(t, b)
	var want []string
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("co-%02d", i)
		newCompany(t, c, name)
		want = append(want, name)
	}
	var got []string
	var sizes []int
	req := crm.PageRequest{Size: 10}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging does not terminate")
		}
		p, err := c.ListCompanies(ctx, crm.CompanyFilter{}, req)
		must(t, err)
		sizes = append(sizes, len(p.Items))
		for _, it := range p.Items {
			got = append(got, it.Name)
		}
		if pages == 0 {
			// A record created mid-iteration must not make existing records repeat or vanish.
			newCompany(t, c, "co-new")
		}
		if p.Next == "" {
			break
		}
		req.Cursor = p.Next
	}
	// The record created after page 1 may or may not be seen by the later pages (keyset paging sees it, a snapshot
	// does not), but pages are full until the last, and every original record is delivered exactly once.
	total := 0
	for i, n := range sizes {
		total += n
		if n > 10 || (i < len(sizes)-1 && n != 10 && i < 2) {
			t.Fatalf("page sizes = %v", sizes)
		}
	}
	if len(sizes) < 3 || sizes[0] != 10 || sizes[1] != 10 || (total != 25 && total != 26) {
		t.Fatalf("page sizes = %v", sizes)
	}
	seen := map[string]int{}
	for _, n := range got {
		seen[n]++
	}
	for _, n := range want {
		if seen[n] != 1 {
			t.Fatalf("%s appeared %d times across pages", n, seen[n])
		}
	}
	if strings.Join(got[:25], ",") != strings.Join(want, ",") {
		t.Fatalf("order is not oldest-created first: %v", got)
	}

	_, err := c.ListCompanies(ctx, crm.CompanyFilter{}, crm.PageRequest{Size: 10, Cursor: "garbage-cursor"})
	isErr(t, err, crm.ErrInvalid, "invalid cursor")

	// A filter composes with paging.
	req = crm.PageRequest{Size: 4}
	var filtered []string
	for {
		p, err := c.ListCompanies(ctx, crm.CompanyFilter{Query: "co-1"}, req)
		must(t, err)
		for _, it := range p.Items {
			filtered = append(filtered, it.Name)
		}
		if p.Next == "" {
			break
		}
		req.Cursor = p.Next
	}
	if len(filtered) != 10 || filtered[0] != "co-10" || filtered[9] != "co-19" {
		t.Fatalf("filtered paging = %v", filtered)
	}
	if p, _ := c.ListCompanies(ctx, crm.CompanyFilter{}, crm.PageRequest{}); len(p.Items) != 26 || p.Next != "" {
		t.Fatalf("default page: %d items, next=%q", len(p.Items), p.Next)
	}
}

// ───────────────────────────── search ─────────────────────────────

func testSearch(t *testing.T, b Backend) {
	c := tenant(t, b)
	for _, n := range []string{"100% Pure Spices", "100x Pure Spices", "a_b Traders", "axb Traders", `back\slash Ltd`, "ACME Foods"} {
		newCompany(t, c, n)
	}
	withDomain, _, err := c.CreateCompany(ctx, key(), crm.CompanyInput{Name: "Domain Co", Domain: "foods.example.com"})
	must(t, err)
	names := func(q string) string {
		p, err := c.ListCompanies(ctx, crm.CompanyFilter{Query: q}, crm.PageRequest{})
		must(t, err)
		var out []string
		for _, it := range p.Items {
			out = append(out, it.Name)
		}
		return strings.Join(out, "|")
	}
	for q, want := range map[string]string{
		"acme":              "ACME Foods",
		"ACME FOODS":        "ACME Foods",
		"100%":              "100% Pure Spices", // % is a literal, not "match anything"
		"a_b":               "a_b Traders",      // _ is a literal, not "any one character"
		`back\slash`:        `back\slash Ltd`,
		"FOODS.EXAMPLE":     withDomain.Name, // domain is searched too
		"nothing-like-this": "",
	} {
		if got := names(q); got != want {
			t.Errorf("query %q = %q, want %q", q, got, want)
		}
	}
	if got := names("pure"); got != "100% Pure Spices|100x Pure Spices" {
		t.Errorf("query pure = %q", got)
	}
}

// ───────────────────────────── malformed ids ─────────────────────────────

func testMalformedIDs(t *testing.T, b Backend) {
	c := tenant(t, b)
	for _, id := range []crm.ID{"", "not-a-uuid", "0", "../../etc/passwd", "'; DROP TABLE crm_companies; --", crm.ID(strings.Repeat("a", 500))} {
		_, err := c.GetCompany(ctx, id)
		isErr(t, err, crm.ErrNotFound, fmt.Sprintf("GetCompany(%q)", id))
		_, err = c.GetDeal(ctx, id)
		isErr(t, err, crm.ErrNotFound, "GetDeal")
		_, err = c.GetContact(ctx, id)
		isErr(t, err, crm.ErrNotFound, "GetContact")
		_, err = c.GetActivity(ctx, id)
		isErr(t, err, crm.ErrNotFound, "GetActivity")
		isErr(t, c.ArchiveCompany(ctx, id, 0), crm.ErrNotFound, "ArchiveCompany")
		_, err = c.UpdateCompany(ctx, id, crm.CompanyPatch{Name: ptr("x")}, 0)
		isErr(t, err, crm.ErrNotFound, "UpdateCompany")
	}
	// As a filter, an id that matches nothing returns nothing rather than an error.
	p, err := c.ListDeals(ctx, crm.DealFilter{CompanyID: "not-a-uuid"}, crm.PageRequest{})
	must(t, err)
	if len(p.Items) != 0 {
		t.Fatalf("filter by a malformed id returned %d deals", len(p.Items))
	}
}

// ───────────────────────────── tenant isolation ─────────────────────────────

func testIsolation(t *testing.T, b Backend) {
	a, bb := tenant(t, b), tenant(t, b)
	coA := newCompany(t, a, "Alpha Co")
	dealA := newDeal(t, a, coA.ID)
	contactA, _, err := a.CreateContact(ctx, key(), crm.ContactInput{CompanyID: coA.ID, Email: "x@alpha.com"})
	must(t, err)
	actA, _, err := a.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityTask, Subject: "t", DealID: dealA.ID})
	must(t, err)

	// B cannot read, change or archive A's records, and cannot tell they exist.
	_, err = bb.GetCompany(ctx, coA.ID)
	isErr(t, err, crm.ErrNotFound, "read another tenant's company")
	_, err = bb.GetDeal(ctx, dealA.ID)
	isErr(t, err, crm.ErrNotFound, "read another tenant's deal")
	_, err = bb.GetContact(ctx, contactA.ID)
	isErr(t, err, crm.ErrNotFound, "read another tenant's contact")
	_, err = bb.GetActivity(ctx, actA.ID)
	isErr(t, err, crm.ErrNotFound, "read another tenant's activity")
	_, err = bb.UpdateCompany(ctx, coA.ID, crm.CompanyPatch{Name: ptr("hacked")}, 0)
	isErr(t, err, crm.ErrNotFound, "update another tenant's company")
	_, err = bb.MoveDeal(ctx, dealA.ID, "won", "x", 0)
	isErr(t, err, crm.ErrNotFound, "move another tenant's deal")
	_, err = bb.CompleteActivity(ctx, actA.ID, "x")
	isErr(t, err, crm.ErrNotFound, "complete another tenant's task")
	isErr(t, bb.ArchiveCompany(ctx, coA.ID, 0), crm.ErrNotFound, "archive another tenant's company")
	if g, err := a.GetCompany(ctx, coA.ID); err != nil || g.Name != "Alpha Co" {
		t.Fatalf("tenant A's record was affected: %+v %v", g, err)
	}

	// B cannot attach its records to A's.
	_, _, err = bb.CreateDeal(ctx, key(), dealInput(coA.ID))
	isErr(t, err, crm.ErrInvalid, "deal on another tenant's company")
	_, _, err = bb.CreateContact(ctx, key(), crm.ContactInput{CompanyID: coA.ID, Email: "y@beta.com"})
	isErr(t, err, crm.ErrInvalid, "contact on another tenant's company")
	_, _, err = bb.CreateActivity(ctx, key(), crm.ActivityInput{Kind: crm.ActivityNote, Subject: "s", DealID: dealA.ID})
	isErr(t, err, crm.ErrInvalid, "activity on another tenant's deal")
	coB := newCompany(t, bb, "Beta Co")
	dealIn := dealInput(coB.ID)
	dealIn.ContactID = contactA.ID
	_, _, err = bb.CreateDeal(ctx, key(), dealIn)
	isErr(t, err, crm.ErrInvalid, "deal with another tenant's contact")

	// Lists are disjoint, and idempotency keys are per tenant.
	if n, _, _, _ := counts(t, bb); n != 1 {
		t.Fatalf("tenant B sees %d companies, want 1", n)
	}
	shared := key()
	x, createdX, err := a.CreateCompany(ctx, shared, crm.CompanyInput{Name: "Same key"})
	must(t, err)
	y, createdY, err := bb.CreateCompany(ctx, shared, crm.CompanyInput{Name: "Same key, other tenant"})
	must(t, err)
	if !createdX || !createdY || x.ID == y.ID {
		t.Fatal("the same idempotency key in two tenants must create two records")
	}

	// Many tenants at once: each sees exactly its own.
	var cs []crm.CRM
	for i := 0; i < 5; i++ {
		c := tenant(t, b)
		for j := 0; j < 3; j++ {
			newCompany(t, c, fmt.Sprintf("t%d-co%d", i, j))
		}
		cs = append(cs, c)
	}
	for i, c := range cs {
		p, _ := c.ListCompanies(ctx, crm.CompanyFilter{}, crm.PageRequest{})
		if len(p.Items) != 3 {
			t.Fatalf("tenant %d sees %d companies", i, len(p.Items))
		}
		for _, it := range p.Items {
			if !strings.HasPrefix(it.Name, fmt.Sprintf("t%d-", i)) {
				t.Fatalf("tenant %d sees %q", i, it.Name)
			}
		}
	}
}

// ───────────────────────────── cancelled context ─────────────────────────────

func testCancelled(t *testing.T, b Backend) {
	c := tenant(t, b)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.CreateCompany(cctx, key(), crm.CompanyInput{Name: "never"}); err == nil {
		t.Fatal("a cancelled context must fail the call")
	}
	if _, err := c.ListCompanies(cctx, crm.CompanyFilter{}, crm.PageRequest{}); err == nil {
		t.Fatal("a cancelled list must fail")
	}
	if n, _, _, _ := counts(t, c); n != 0 {
		t.Fatalf("a cancelled create left %d records", n)
	}
}

// ───────────────────────────── unicode ─────────────────────────────

func testUnicode(t *testing.T, b Backend) {
	c := tenant(t, b)
	// 150 Devanagari characters is 450 bytes: the limit is in characters.
	hindi := strings.Repeat("मसाला", 30)
	co, _, err := c.CreateCompany(ctx, key(), crm.CompanyInput{Name: hindi, Industry: "मसाले 🌶️"})
	must(t, err)
	got, err := c.GetCompany(ctx, co.ID)
	must(t, err)
	if got.Name != hindi || got.Industry != "मसाले 🌶️" {
		t.Fatalf("unicode did not round-trip: %q", got.Name)
	}
	if _, _, err := c.CreateCompany(ctx, key(), crm.CompanyInput{Name: strings.Repeat("é", crm.MaxNameLen)}); err != nil {
		t.Fatalf("exactly the limit in characters must be accepted: %v", err)
	}
	_, _, err = c.CreateCompany(ctx, key(), crm.CompanyInput{Name: strings.Repeat("é", crm.MaxNameLen+1)})
	isErr(t, err, crm.ErrInvalid, "one character over the limit")
	p, _ := c.ListCompanies(ctx, crm.CompanyFilter{Query: "मसाला"}, crm.PageRequest{})
	if len(p.Items) != 1 {
		t.Fatalf("unicode search found %d", len(p.Items))
	}
}

// ───────────────────────────── aliasing ─────────────────────────────

func testCopies(t *testing.T, b Backend) {
	c := tenant(t, b)
	in := crm.CompanyInput{Name: "Copy", Fields: map[string]string{"a": "1"}}
	co, _, err := c.CreateCompany(ctx, key(), in)
	must(t, err)
	in.Fields["a"] = "mutated-after-create"
	co.Fields["a"] = "mutated-result"
	g1, _ := c.GetCompany(ctx, co.ID)
	if g1.Fields["a"] != "1" {
		t.Fatalf("the backend aliased caller memory: %v", g1.Fields)
	}
	g1.Fields["a"] = "mutated-get"
	if g2, _ := c.GetCompany(ctx, co.ID); g2.Fields["a"] != "1" {
		t.Fatal("a returned record shares memory with the store")
	}
}
