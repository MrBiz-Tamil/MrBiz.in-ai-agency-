package controlplane_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	crmv1 "github.com/mrbiz/venture-os/gen/venture/crm/v1"
	"github.com/mrbiz/venture-os/gen/venture/crm/v1/crmv1connect"
	"github.com/mrbiz/venture-os/internal/crmops"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

func (e *env) crm(token string) crmv1connect.CrmServiceClient {
	return crmv1connect.NewCrmServiceClient(http.DefaultClient, e.url, connect.WithInterceptors(bearer(token)))
}

func runOp(t *testing.T, c crmv1connect.CrmServiceClient, tenant, op string, args any) (map[string]any, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	r, err := c.Execute(ctx, connect.NewRequest(&crmv1.ExecuteRequest{TenantId: tenant, Operation: op, ArgsJson: raw}))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(r.Msg.ResultJson, &m); err != nil {
		t.Fatalf("%s: result is not a JSON object: %s", op, r.Msg.ResultJson)
	}
	return m, nil
}

func TestPeopleUseTheCRMWithRoleChecksAndAttribution(t *testing.T) {
	e := newEnv(t)
	a := goLive(t, e, "tok-founder", "kerala-spice-exports")
	other := goLive(t, e, "tok-other", "globex-exim")
	must := func(m map[string]any, err error) map[string]any {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for sub, role := range map[string]string{"sub-viewer": "viewer", "sub-approver": "approver"} {
		if err := e.store.AddMember(ctx, a.uuid, sub, role); err != nil {
			t.Fatal(err)
		}
	}
	founder, approver, viewer, outsider := e.crm("tok-founder"), e.crm("tok-approver"), e.crm("tok-viewer"), e.crm("tok-other")

	company := must(runOp(t, founder, a.id, "crm_create_company", map[string]any{"name": "Nordic Spice Importers AB", "idempotency_key": "c1"}))["company"].(map[string]any)
	deal := must(runOp(t, founder, a.id, "crm_create_deal", map[string]any{
		"name": "Pepper", "company_id": company["id"], "pipeline": "export_deals", "stage": "inquiry",
		"fields": map[string]string{"incoterm": "FOB"}, "idempotency_key": "d1"}))["deal"].(map[string]any)

	// Agents cannot reach the human-only operations at all, not even by name.
	for _, name := range []string{"crm_archive_deal", "crm_update_company", "crm_update_contact", "crm_archive_company", "crm_archive_contact"} {
		_, err := a.agent.CRMOperation(ctx, connect.NewRequest(&agentv1.CRMOperationRequest{Operation: name, ArgsJson: []byte(`{}`)}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "unknown CRM operation") {
			t.Errorf("an agent calling %s must be told it does not exist, got %v", name, err)
		}
	}

	// An approver may write, and the row says who.
	must(runOp(t, approver, a.id, "crm_add_activity", map[string]any{"kind": "NOTE", "subject": "Called the buyer", "deal_id": deal["id"], "idempotency_key": "n1"}))

	// A viewer can read everything but write nothing.
	if m, err := runOp(t, viewer, a.id, "crm_find_deals", map[string]any{}); err != nil || len(m["deals"].(map[string]any)["items"].([]any)) != 1 {
		t.Fatalf("viewer read: %v %v", m, err)
	}
	if _, err := runOp(t, viewer, a.id, "crm_get_company", map[string]any{"company_id": company["id"]}); err != nil {
		t.Fatalf("viewer get_company: %v", err)
	}
	for _, op := range crmops.All() {
		if op.ReadOnly {
			continue
		}
		if _, err := runOp(t, viewer, a.id, op.Name, map[string]any{}); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("a viewer calling %s must be PermissionDenied, got %v", op.Name, err)
		}
	}

	// A founder's move is attributed to the person, not to an agent.
	must(runOp(t, founder, a.id, "crm_move_deal", map[string]any{"deal_id": deal["id"], "stage": "quoted"}))
	got := must(runOp(t, viewer, a.id, "crm_get_deal", map[string]any{"deal_id": deal["id"]}))
	hist := got["stage_history"].([]any)
	if by := hist[len(hist)-1].(map[string]any)["changed_by"]; by != "user:sub-founder" {
		t.Fatalf("changed_by = %v, want user:sub-founder", by)
	}

	// Another business's member cannot tell this one exists, whatever the operation.
	for _, op := range []string{"crm_find_deals", "crm_pipelines", "crm_create_company"} {
		if _, err := runOp(t, outsider, a.id, op, map[string]any{"name": "x", "idempotency_key": "k"}); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("an outsider calling %s must get NotFound, got %v", op, err)
		}
	}
	// ...and a member of both cannot reach A's records through B's tenant id.
	if _, err := runOp(t, outsider, other.id, "crm_get_deal", map[string]any{"deal_id": deal["id"]}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a deal id from another business must be NotFound, got %v", err)
	}

	// Bad requests: unknown operation, smuggled argument, malformed tenant.
	if _, err := runOp(t, founder, a.id, "crm_drop_everything", map[string]any{}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown operation: %v", err)
	}
	if _, err := runOp(t, founder, a.id, "crm_find_deals", map[string]any{"owner": "x"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown argument: %v", err)
	}
	if _, err := runOp(t, founder, "not-a-uuid", "crm_find_deals", map[string]any{}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("malformed tenant: %v", err)
	}
	// A stale version is a clear, actionable failure.
	_, err := runOp(t, founder, a.id, "crm_move_deal", map[string]any{"deal_id": deal["id"], "stage": "shipped", "expected_version": 1})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "fetch it again") {
		t.Errorf("stale version: %v", err)
	}

	// Writes are audited under the person; reads and refused calls are not.
	var actions []string
	err = tenancy.WithScopeTx(ctx, e.pool, tenancy.Scope{TenantID: a.uuid}, tenancy.Options{AssumeRole: testdb.Role}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action || '/' || actor_id FROM audit_logs WHERE action LIKE 'crm.%' AND actor_type = 'USER' ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			actions = append(actions, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "crm.crm_create_company/sub-founder,crm.crm_create_deal/sub-founder,crm.crm_add_activity/sub-approver,crm.crm_move_deal/sub-founder"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit = %v, want %s", actions, want)
	}
}
