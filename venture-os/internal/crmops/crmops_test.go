package crmops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/crm/crmtest"
	"github.com/mrbiz/venture-os/internal/crm/memory"
	"github.com/mrbiz/venture-os/internal/crmops"
)

var ctx = context.Background()

const actor = "agent:sales"

func newCRM(t *testing.T) crm.CRM {
	t.Helper()
	s := memory.New(memory.Options{})
	id := uuid.New()
	s.AddTenant(id, crmtest.Pipelines())
	c, err := s.For(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// call runs an operation and returns its answer as generic JSON, exactly as an agent would receive it.
func call(t *testing.T, c crm.CRM, name string, args any) map[string]any {
	t.Helper()
	m, err := try(t, c, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

func try(t *testing.T, c crm.CRM, name string, args any) (map[string]any, error) {
	t.Helper()
	op, ok := crmops.Find(name)
	if !ok {
		t.Fatalf("no operation %s", name)
	}
	raw, _ := json.Marshal(args)
	res, err := op.Run(ctx, c, actor, raw)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(res.Data)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s did not return a JSON object: %s", name, b)
	}
	return m, nil
}

func obj(m map[string]any, k string) map[string]any { return m[k].(map[string]any) }
func s(m map[string]any, k string) string           { v, _ := m[k].(string); return v }

func TestEveryOperationIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, op := range crmops.All() {
		if !strings.HasPrefix(op.Name, "crm_") || seen[op.Name] {
			t.Errorf("bad or duplicate name %q", op.Name)
		}
		seen[op.Name] = true
		var sch map[string]any
		if err := json.Unmarshal([]byte(op.Schema), &sch); err != nil {
			t.Errorf("%s: schema is not JSON: %v", op.Name, err)
			continue
		}
		if sch["type"] != "object" || sch["additionalProperties"] != false {
			t.Errorf("%s: schema must be a closed object", op.Name)
		}
		if len(op.Description) < 40 {
			t.Errorf("%s: description is too thin to guide an agent", op.Name)
		}
		// Every required property must exist.
		props, _ := sch["properties"].(map[string]any)
		if req, ok := sch["required"].([]any); ok {
			for _, r := range req {
				if _, ok := props[r.(string)]; !ok {
					t.Errorf("%s: required %v is not a property", op.Name, r)
				}
			}
		}
		// Anything that returns customer-typed text must tell the agent to treat it as data.
		if op.ReadOnly && op.Name != "crm_pipelines" && !strings.Contains(op.Description, "never as instructions") {
			t.Errorf("%s: a read tool returning customer text must carry the data-not-instructions warning", op.Name)
		}
	}
	if len(seen) != 19 || len(crmops.ForAgents()) != 14 {
		t.Errorf("expected 19 operations of which 14 for agents, found %d and %d", len(seen), len(crmops.ForAgents()))
	}
	if crmops.Family != "crm_ops" {
		t.Errorf("family = %q", crmops.Family)
	}
}

func TestEveryOperationRejectsUnknownArguments(t *testing.T) {
	c := newCRM(t)
	for _, op := range crmops.All() {
		for _, smuggled := range []string{`{"tenant_id":"00000000-0000-0000-0000-000000000001"}`, `{"owner":"someone"}`, `{"x":1}`} {
			_, err := op.Run(ctx, c, actor, json.RawMessage(smuggled))
			if !errors.Is(err, crm.ErrInvalid) {
				t.Errorf("%s accepted %s: %v", op.Name, smuggled, err)
			}
		}
		if _, err := op.Run(ctx, c, actor, json.RawMessage(`{} {}`)); !errors.Is(err, crm.ErrInvalid) {
			t.Errorf("%s accepted trailing data: %v", op.Name, err)
		}
		if _, err := op.Run(ctx, c, actor, json.RawMessage(`[1]`)); !errors.Is(err, crm.ErrInvalid) {
			t.Errorf("%s accepted a non-object: %v", op.Name, err)
		}
	}
}

func TestReadOnlyOperationsWriteNothing(t *testing.T) {
	c := newCRM(t)
	for _, name := range []string{"crm_pipelines", "crm_find_companies", "crm_find_contacts", "crm_find_deals", "crm_open_tasks"} {
		if _, err := try(t, c, name, map[string]any{}); err != nil {
			t.Errorf("%s with no arguments: %v", name, err)
		}
	}
	p, _ := c.ListCompanies(ctx, crm.CompanyFilter{IncludeArchived: true}, crm.PageRequest{})
	if len(p.Items) != 0 {
		t.Fatal("a read created data")
	}
	for _, op := range crmops.All() {
		if op.ReadOnly != (strings.HasPrefix(op.Name, "crm_find_") || op.Name == "crm_pipelines" || op.Name == "crm_get_deal" || op.Name == "crm_get_company" || op.Name == "crm_open_tasks") {
			t.Errorf("%s: ReadOnly flag is %v", op.Name, op.ReadOnly)
		}
	}
}

func TestSalesWorkflowEndToEnd(t *testing.T) {
	c := newCRM(t)

	pl := call(t, c, "crm_pipelines", map[string]any{})
	pipes := pl["pipelines"].([]any)
	if len(pipes) != 2 || s(pipes[0].(map[string]any), "key") != "sales" {
		t.Fatalf("pipelines = %v", pl)
	}

	// A company, created twice with the same key (an agent retrying) is one company.
	co1 := call(t, c, "crm_create_company", map[string]any{"name": "Nordic Spice Importers AB", "domain": "NordicSpice.se", "country": "se", "idempotency_key": "co-1"})
	co2 := call(t, c, "crm_create_company", map[string]any{"name": "Nordic Spice Importers AB", "domain": "NordicSpice.se", "country": "se", "idempotency_key": "co-1"})
	if co1["created"] != true || co2["created"] != false || s(obj(co1, "company"), "id") != s(obj(co2, "company"), "id") {
		t.Fatalf("retry was not idempotent: %v / %v", co1, co2)
	}
	company := obj(co1, "company")
	if company["domain"] != "nordicspice.se" || company["country"] != "SE" {
		t.Fatalf("not normalized: %v", company)
	}
	companyID := s(company, "id")

	found := call(t, c, "crm_find_companies", map[string]any{"query": "NORDIC"})
	if items := obj(found, "companies")["items"].([]any); len(items) != 1 {
		t.Fatalf("find: %v", found)
	}

	ct := call(t, c, "crm_create_contact", map[string]any{"company_id": companyID, "first_name": "Astrid", "email": "Astrid@NordicSpice.se", "idempotency_key": "ct-1"})
	contactID := s(obj(ct, "contact"), "id")
	if obj(ct, "contact")["email"] != "astrid@nordicspice.se" {
		t.Fatalf("contact: %v", ct)
	}

	d := call(t, c, "crm_create_deal", map[string]any{
		"name": "Black pepper 2 x 20ft", "company_id": companyID, "contact_id": contactID, "pipeline": "sales", "stage": "lead",
		"amount_minor": 4800000, "currency": "USD", "expected_close": "2026-04-30",
		"fields": map[string]string{"incoterm": "CIF", "destination": "Gothenburg"}, "idempotency_key": "deal-1",
	})
	deal := obj(d, "deal")
	dealID := s(deal, "id")
	if obj(deal, "amount")["amount_minor"].(float64) != 4800000 || deal["expected_close"] != "2026-04-30" || deal["stage"] != "lead" {
		t.Fatalf("deal: %v", deal)
	}
	version := int64(deal["version"].(float64))

	moved := call(t, c, "crm_move_deal", map[string]any{"deal_id": dealID, "stage": "qualified", "expected_version": version})
	if obj(moved, "deal")["stage"] != "qualified" {
		t.Fatalf("move: %v", moved)
	}

	// The same stale version cannot be used again: no silent overwrite.
	_, err := try(t, c, "crm_move_deal", map[string]any{"deal_id": dealID, "stage": "quoted", "expected_version": version})
	if !errors.Is(err, crm.ErrConflict) {
		t.Fatalf("a stale move must conflict, got %v", err)
	}
	_, err = try(t, c, "crm_update_deal", map[string]any{"deal_id": dealID, "name": "stale", "expected_version": version})
	if !errors.Is(err, crm.ErrConflict) {
		t.Fatalf("a stale update must conflict, got %v", err)
	}

	up := call(t, c, "crm_update_deal", map[string]any{"deal_id": dealID, "name": "Black pepper, 2 containers", "clear_expected_close": true,
		"fields": map[string]string{"destination": ""}})
	udeal := obj(up, "deal")
	if udeal["name"] != "Black pepper, 2 containers" || udeal["expected_close"] != nil || udeal["amount"] == nil {
		t.Fatalf("update: %v", udeal)
	}
	if _, has := udeal["fields"].(map[string]any)["destination"]; has {
		t.Fatal("optional field was not removed")
	}

	call(t, c, "crm_add_activity", map[string]any{"kind": "CALL", "subject": "Intro call", "body": "Wants samples.", "deal_id": dealID, "idempotency_key": "act-1"})
	task := call(t, c, "crm_add_activity", map[string]any{"kind": "TASK", "subject": "Send samples", "due": "2026-03-10", "deal_id": dealID, "idempotency_key": "act-2"})
	taskID := s(obj(task, "activity"), "id")
	if obj(task, "activity")["created_by"] != actor || obj(task, "activity")["due_at"] != "2026-03-10T00:00:00Z" {
		t.Fatalf("task: %v", task)
	}

	open := call(t, c, "crm_open_tasks", map[string]any{"deal_id": dealID})
	if items := obj(open, "tasks")["items"].([]any); len(items) != 1 {
		t.Fatalf("open tasks: %v", open)
	}
	call(t, c, "crm_complete_task", map[string]any{"activity_id": taskID})
	call(t, c, "crm_complete_task", map[string]any{"activity_id": taskID}) // idempotent
	open = call(t, c, "crm_open_tasks", map[string]any{"deal_id": dealID})
	if items := obj(open, "tasks")["items"].([]any); len(items) != 0 {
		t.Fatalf("a completed task is still open: %v", open)
	}

	got := call(t, c, "crm_get_deal", map[string]any{"deal_id": dealID})
	if obj(got, "company")["name"] != "Nordic Spice Importers AB" {
		t.Fatalf("company missing from get_deal: %v", got)
	}
	hist := got["stage_history"].([]any)
	if len(hist) != 2 || hist[0].(map[string]any)["changed_by"] != actor || hist[1].(map[string]any)["changed_by"] != actor || hist[1].(map[string]any)["to"] != "qualified" {
		t.Fatalf("history must record the agent: %v", hist)
	}
	if acts := got["activities"].([]any); len(acts) != 2 || got["activities_truncated"] != false {
		t.Fatalf("activities: %v", got["activities"])
	}

	gc := call(t, c, "crm_get_company", map[string]any{"company_id": companyID})
	if len(gc["contacts"].([]any)) != 1 || len(gc["deals"].([]any)) != 1 || len(gc["open_tasks"].([]any)) != 0 || gc["deals_truncated"] != false {
		t.Fatalf("get_company: %v", gc)
	}
	if _, err := try(t, c, "crm_get_company", map[string]any{"company_id": uuid.NewString()}); !errors.Is(err, crm.ErrNotFound) {
		t.Fatalf("unknown company: %v", err)
	}

	byStage := call(t, c, "crm_find_deals", map[string]any{"pipeline": "sales", "stage": "qualified"})
	if items := obj(byStage, "deals")["items"].([]any); len(items) != 1 {
		t.Fatalf("find by stage: %v", byStage)
	}
}

func TestBadInputsAreReportedNotApplied(t *testing.T) {
	c := newCRM(t)
	co := obj(call(t, c, "crm_create_company", map[string]any{"name": "Acme", "idempotency_key": "k"}), "company")
	id := s(co, "id")
	okDeal := func() map[string]any {
		return map[string]any{"name": "d", "company_id": id, "pipeline": "sales", "stage": "lead", "fields": map[string]string{"incoterm": "FOB"}, "idempotency_key": uuid.NewString()}
	}
	cases := map[string]struct {
		op   string
		args map[string]any
		want error
	}{
		"amount without currency":   {"crm_create_deal", with(okDeal(), "amount_minor", 5), crm.ErrInvalid},
		"currency without amount":   {"crm_create_deal", with(okDeal(), "currency", "USD"), crm.ErrInvalid},
		"bad date":                  {"crm_create_deal", with(okDeal(), "expected_close", "31/03/2026"), crm.ErrInvalid},
		"unknown stage":             {"crm_create_deal", with(okDeal(), "stage", "shipped"), crm.ErrInvalid},
		"missing required field":    {"crm_create_deal", with(okDeal(), "fields", map[string]string{}), crm.ErrInvalid},
		"unknown company":           {"crm_create_deal", with(okDeal(), "company_id", uuid.NewString()), crm.ErrInvalid},
		"no idempotency key":        {"crm_create_company", map[string]any{"name": "x"}, crm.ErrInvalid},
		"limit too large":           {"crm_find_companies", map[string]any{"limit": 51}, crm.ErrInvalid},
		"bad cursor":                {"crm_find_companies", map[string]any{"cursor": "junk"}, crm.ErrInvalid},
		"task due date on a note":   {"crm_add_activity", map[string]any{"kind": "NOTE", "subject": "s", "due": "2026-03-01", "company_id": id, "idempotency_key": "a"}, crm.ErrInvalid},
		"bad kind":                  {"crm_add_activity", map[string]any{"kind": "EMAIL_SEND", "subject": "s", "company_id": id, "idempotency_key": "b"}, crm.ErrInvalid},
		"activity without a parent": {"crm_add_activity", map[string]any{"kind": "NOTE", "subject": "s", "idempotency_key": "c"}, crm.ErrInvalid},
		"bad due":                   {"crm_add_activity", map[string]any{"kind": "TASK", "subject": "s", "due": "soon", "company_id": id, "idempotency_key": "d"}, crm.ErrInvalid},
		"unknown deal":              {"crm_get_deal", map[string]any{"deal_id": uuid.NewString()}, crm.ErrNotFound},
		"malformed deal id":         {"crm_move_deal", map[string]any{"deal_id": "../../x", "stage": "won"}, crm.ErrNotFound},
		"complete a note":           {"crm_complete_task", map[string]any{"activity_id": "nope"}, crm.ErrNotFound},
		"same key different input":  {"crm_create_company", map[string]any{"name": "Different", "idempotency_key": "k"}, crm.ErrIdempotencyMismatch},
	}
	for name, tc := range cases {
		if _, err := try(t, c, tc.op, tc.args); !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, err)
		}
	}
	if p, _ := c.ListDeals(ctx, crm.DealFilter{IncludeArchived: true}, crm.PageRequest{}); len(p.Items) != 0 {
		t.Fatalf("rejected calls created %d deals", len(p.Items))
	}
}

func with(m map[string]any, kv ...any) map[string]any {
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestLongBodiesAreTruncatedForTheAgentButNotInTheCRM(t *testing.T) {
	c := newCRM(t)
	co := obj(call(t, c, "crm_create_company", map[string]any{"name": "Acme", "idempotency_key": "k"}), "company")
	long := strings.Repeat("मसाला ", 1000) // 6000 characters
	a := obj(call(t, c, "crm_add_activity", map[string]any{"kind": "EMAIL", "subject": "Long thread", "body": long, "company_id": s(co, "id"), "idempotency_key": "a"}), "activity")
	if a["body_truncated"] != true || len([]rune(a["body"].(string))) != 2000 {
		t.Fatalf("body not truncated for the agent: %v chars, truncated=%v", len([]rune(a["body"].(string))), a["body_truncated"])
	}
	stored, err := c.GetActivity(ctx, crm.ID(s(a, "id")))
	if err != nil || stored.Body != strings.TrimSpace(long) {
		t.Fatalf("the CRM must keep the full text (%d chars, %v)", len([]rune(stored.Body)), err)
	}
}

func TestPagingContinuesWithTheCursor(t *testing.T) {
	c := newCRM(t)
	for i := 0; i < 25; i++ {
		call(t, c, "crm_create_company", map[string]any{"name": fmt.Sprintf("co-%02d", i), "idempotency_key": fmt.Sprintf("k%d", i)})
	}
	var names []string
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		args := map[string]any{"limit": 10}
		if cursor != "" {
			args["cursor"] = cursor
		}
		p := obj(call(t, c, "crm_find_companies", args), "companies")
		for _, it := range p["items"].([]any) {
			names = append(names, s(it.(map[string]any), "name"))
		}
		cursor = s(p, "next_cursor")
		if cursor == "" {
			break
		}
	}
	if len(names) != 25 || names[0] != "co-00" || names[24] != "co-24" {
		t.Fatalf("paged %d names: %v", len(names), names)
	}
}

func TestAgentsCannotAssignOwnersOrReachArchive(t *testing.T) {
	// These are policy, not accident: nothing an agent can call names an owner, archives, deletes or rewrites the
	// identity of a customer record.
	for _, op := range crmops.ForAgents() {
		if strings.Contains(op.Schema, `"owner`) {
			t.Errorf("%s exposes an owner argument", op.Name)
		}
		for _, banned := range []string{"archive", "delete", "update_company", "update_contact"} {
			if strings.Contains(op.Name, banned) {
				t.Errorf("%s: agents must not use %s", op.Name, banned)
			}
		}
	}
	// ...and the human-only set is exactly what we mean it to be, so adding an operation cannot quietly widen it.
	var human []string
	for _, op := range crmops.All() {
		if op.HumanOnly {
			human = append(human, op.Name)
		}
	}
	if strings.Join(human, ",") != "crm_update_company,crm_update_contact,crm_archive_company,crm_archive_contact,crm_archive_deal" {
		t.Errorf("human-only operations = %v", human)
	}
}

func TestPeopleCanEditAndArchiveWithVersionChecks(t *testing.T) {
	c := newCRM(t)
	co := obj(call(t, c, "crm_create_company", map[string]any{"name": "Acme", "country": "DE", "idempotency_key": "k1"}), "company")
	id, v := s(co, "id"), co["version"]

	up := obj(call(t, c, "crm_update_company", map[string]any{"company_id": id, "name": "Acme GmbH", "domain": "ACME.de", "expected_version": v}), "company")
	if up["name"] != "Acme GmbH" || up["domain"] != "acme.de" || up["country"] != "DE" {
		t.Fatalf("update: %v", up)
	}
	if _, err := try(t, c, "crm_update_company", map[string]any{"company_id": id, "name": "stale", "expected_version": v}); !errors.Is(err, crm.ErrConflict) {
		t.Fatalf("a stale edit must conflict: %v", err)
	}
	if _, err := try(t, c, "crm_update_company", map[string]any{"company_id": id, "country": "Germany"}); !errors.Is(err, crm.ErrInvalid) {
		t.Fatalf("invalid country: %v", err)
	}

	ct := obj(call(t, c, "crm_create_contact", map[string]any{"company_id": id, "first_name": "Ada", "email": "ada@acme.de", "idempotency_key": "k2"}), "contact")
	upc := obj(call(t, c, "crm_update_contact", map[string]any{"contact_id": s(ct, "id"), "title": "Buyer", "last_name": "Lovelace"}), "contact")
	if upc["title"] != "Buyer" || upc["last_name"] != "Lovelace" || upc["first_name"] != "Ada" || upc["company_id"] != id {
		t.Fatalf("contact update: %v", upc)
	}
	deal := obj(call(t, c, "crm_create_deal", map[string]any{"name": "d", "company_id": id, "pipeline": "sales", "stage": "lead", "fields": map[string]string{"incoterm": "FOB"}, "idempotency_key": "k3"}), "deal")

	// Archiving hides each record from reads and lists, and says what was archived.
	for _, tc := range []struct{ op, id, find, list, key string }{
		{"crm_archive_deal", s(deal, "id"), "crm_get_deal", "crm_find_deals", "deals"},
		{"crm_archive_contact", s(ct, "id"), "crm_find_contacts", "crm_find_contacts", "contacts"},
		{"crm_archive_company", id, "crm_get_company", "crm_find_companies", "companies"},
	} {
		r := call(t, c, tc.op, map[string]any{"id": tc.id})
		if r["archived"] != true || r["id"] != tc.id {
			t.Fatalf("%s: %v", tc.op, r)
		}
		if _, err := try(t, c, tc.op, map[string]any{"id": tc.id}); !errors.Is(err, crm.ErrNotFound) {
			t.Fatalf("%s twice: %v", tc.op, err)
		}
		if items := obj(call(t, c, tc.list, map[string]any{}), tc.key)["items"].([]any); len(items) != 0 {
			t.Fatalf("%s: archived records are still listed: %v", tc.op, items)
		}
	}
	if _, err := try(t, c, "crm_get_company", map[string]any{"company_id": id}); !errors.Is(err, crm.ErrNotFound) {
		t.Fatalf("an archived company must read as not found: %v", err)
	}
	// A stale version stops an archive too.
	c2 := obj(call(t, c, "crm_create_company", map[string]any{"name": "Beta", "idempotency_key": "k4"}), "company")
	if _, err := try(t, c, "crm_archive_company", map[string]any{"id": s(c2, "id"), "expected_version": 99}); !errors.Is(err, crm.ErrConflict) {
		t.Fatalf("stale archive: %v", err)
	}
	if _, err := try(t, c, "crm_get_company", map[string]any{"company_id": s(c2, "id")}); err != nil {
		t.Fatal("a refused archive removed the record")
	}
}
