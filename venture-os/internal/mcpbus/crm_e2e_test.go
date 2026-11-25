package mcpbus_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	"github.com/mrbiz/venture-os/internal/mcpbus"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
	"github.com/mrbiz/venture-os/internal/testenv"
)

// newCRME2E is newE2E with the crm_ops tools switched on or off on the bus, and optionally no CRM behind the control plane.
func newCRME2E(t *testing.T, busOffersCRM bool, opts ...testenv.Option) *e2e {
	t.Helper()
	stack := testenv.New(t, opts...)
	agent := agentv1connect.NewAgentServiceClient(http.DefaultClient, stack.URL)
	bus := httptest.NewServer(mcpbus.New(mcpbus.Server{Agent: agent, Signer: stack.Signer, EnableCRM: busOffersCRM}))
	t.Cleanup(bus.Close)
	return &e2e{stack: stack, bus: bus}
}

func (e *e2e) toolNames(t *testing.T, token string) []string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req, _ := http.NewRequest(http.MethodPost, e.bus.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r rpc
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnly bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range out.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func countPrefix(names []string, p string) int {
	n := 0
	for _, s := range names {
		if strings.HasPrefix(s, p) {
			n++
		}
	}
	return n
}

func TestCRMToolsAreOfferedOnlyWhenThereIsACRM(t *testing.T) {
	off := newCRME2E(t, false)
	b := off.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	if names := off.toolNames(t, b.AgentToken); countPrefix(names, "crm_") != 0 || len(names) != 9 {
		t.Fatalf("with no CRM the agent must see only the 9 standard tools, got %v", names)
	}
	on := newCRME2E(t, true)
	b2 := on.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	if names := on.toolNames(t, b2.AgentToken); countPrefix(names, "crm_") != 14 || len(names) != 23 {
		t.Fatalf("with a CRM the agent must see 14 crm_ tools beside the 9, got %d: %v", len(names), names)
	}
}

// If a bus is told to offer the tools but the control plane has no CRM, the call fails in words an agent can act on.
func TestCRMToolFailsCleanlyWhenTheControlPlaneHasNoCRM(t *testing.T) {
	e := newCRME2E(t, true, testenv.WithoutCRM())
	b := e.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	text, isErr := e.call(t, b.AgentToken, "crm_find_companies", map[string]any{})
	if !isErr || !strings.Contains(text, "no CRM connected") {
		t.Fatalf("got isErr=%v %q", isErr, text)
	}
}

func TestAgentRunsASalesWorkflowThroughMCP(t *testing.T) {
	e := newCRME2E(t, true)
	a := e.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	other := e.stack.LiveBusiness(t, "tok-other", "globex-exim")
	ok := func(token, tool string, args map[string]any) map[string]any {
		t.Helper()
		text, isErr := e.call(t, token, tool, args)
		if isErr {
			t.Fatalf("%s: %s", tool, text)
		}
		return decode(t, text)
	}

	// The blueprint's own pipelines are what the agent sees.
	pl := ok(a.AgentToken, "crm_pipelines", map[string]any{})["pipelines"].([]any)
	if len(pl) != 2 || pl[0].(map[string]any)["key"] != "export_deals" {
		t.Fatalf("pipelines = %v", pl)
	}

	co := ok(a.AgentToken, "crm_create_company", map[string]any{"name": "Nordic Spice Importers AB", "country": "SE", "idempotency_key": "co-1"})["company"].(map[string]any)
	deal := ok(a.AgentToken, "crm_create_deal", map[string]any{
		"name": "Black pepper 2 x 20ft", "company_id": co["id"], "pipeline": "export_deals", "stage": "inquiry",
		"amount_minor": 4800000, "currency": "USD", "fields": map[string]string{"incoterm": "CIF"}, "idempotency_key": "deal-1",
	})["deal"].(map[string]any)
	dealID := deal["id"].(string)

	moved := ok(a.AgentToken, "crm_move_deal", map[string]any{"deal_id": dealID, "stage": "quoted", "expected_version": deal["version"]})["deal"].(map[string]any)
	if moved["stage"] != "quoted" {
		t.Fatalf("moved = %v", moved)
	}
	ok(a.AgentToken, "crm_add_activity", map[string]any{"kind": "TASK", "subject": "Send samples", "due": "2026-03-10", "deal_id": dealID, "idempotency_key": "t-1"})

	// A stale write is refused in words the agent can act on.
	text, isErr := e.call(t, a.AgentToken, "crm_move_deal", map[string]any{"deal_id": dealID, "stage": "sample_sent", "expected_version": deal["version"]})
	if !isErr || !strings.Contains(text, "fetch it again") {
		t.Fatalf("stale move: isErr=%v %q", isErr, text)
	}
	// So is a stage the blueprint does not have, and a smuggled tenant.
	if text, isErr = e.call(t, a.AgentToken, "crm_move_deal", map[string]any{"deal_id": dealID, "stage": "delivered"}); !isErr || !strings.Contains(text, "no stage") {
		t.Fatalf("unknown stage: isErr=%v %q", isErr, text)
	}
	if text, isErr = e.call(t, a.AgentToken, "crm_find_deals", map[string]any{"tenant_id": other.ID}); !isErr || !strings.Contains(text, "unknown field") {
		t.Fatalf("a smuggled tenant_id must be refused: isErr=%v %q", isErr, text)
	}

	got := ok(a.AgentToken, "crm_get_deal", map[string]any{"deal_id": dealID})
	hist := got["stage_history"].([]any)
	if len(hist) != 2 || hist[1].(map[string]any)["changed_by"] != "agent:sales" {
		t.Fatalf("history = %v", hist)
	}

	// The other business, through its own token, cannot see or touch any of it.
	for tool, args := range map[string]map[string]any{
		"crm_get_deal":  {"deal_id": dealID},
		"crm_move_deal": {"deal_id": dealID, "stage": "shipped"},
	} {
		if text, isErr := e.call(t, other.AgentToken, tool, args); !isErr || !strings.Contains(text, "not found") {
			t.Errorf("%s across tenants: isErr=%v %q", tool, isErr, text)
		}
	}
	if deals := ok(other.AgentToken, "crm_find_deals", map[string]any{})["deals"].(map[string]any)["items"].([]any); len(deals) != 0 {
		t.Fatalf("the other business sees %d deals", len(deals))
	}
	if _, isErr := e.call(t, other.AgentToken, "crm_create_deal", map[string]any{
		"name": "x", "company_id": co["id"], "pipeline": "export_deals", "stage": "inquiry", "fields": map[string]string{"incoterm": "FOB"}, "idempotency_key": "x",
	}); !isErr {
		t.Fatal("a deal on another business's company must be refused")
	}
	if g := ok(a.AgentToken, "crm_get_deal", map[string]any{"deal_id": dealID})["deal"].(map[string]any); g["stage"] != "quoted" {
		t.Fatalf("the other business changed this deal: %v", g)
	}

	// Writes are audited with the agent as the actor, and the audit rows carry no business data.
	type row struct{ actor, action, entity, data string }
	var rows []row
	err := tenancy.WithScopeTx(context.Background(), e.stack.Pool, tenancy.Scope{TenantID: a.UUID}, tenancy.Options{AssumeRole: testdb.Role}, func(tx pgx.Tx) error {
		rs, err := tx.Query(context.Background(),
			`SELECT actor_id, action, coalesce(entity_id,''), data::text FROM audit_logs WHERE action LIKE 'crm.%' AND actor_type = 'AGENT' ORDER BY id`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.actor, &r.action, &r.entity, &r.data); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.action)
		if r.actor != "agent:sales" || r.entity == "" || r.data != "{}" {
			t.Errorf("bad audit row %+v", r)
		}
	}
	want := "crm.crm_create_company,crm.crm_create_deal,crm.crm_move_deal,crm.crm_add_activity"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audited actions = %v, want %s (reads and failed calls are not audited as writes)", actions, want)
	}
}
