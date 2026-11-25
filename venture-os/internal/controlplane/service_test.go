package controlplane_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	genesisv1 "github.com/mrbiz/venture-os/gen/venture/genesis/v1"
	"github.com/mrbiz/venture-os/gen/venture/genesis/v1/genesisv1connect"
	provisionerv1 "github.com/mrbiz/venture-os/gen/venture/provisioner/v1"
	"github.com/mrbiz/venture-os/gen/venture/provisioner/v1/provisionerv1connect"
	tenantv1 "github.com/mrbiz/venture-os/gen/venture/tenant/v1"
	"github.com/mrbiz/venture-os/gen/venture/tenant/v1/tenantv1connect"
	"github.com/mrbiz/venture-os/internal/auth"
	"github.com/mrbiz/venture-os/internal/controlplane"
	"github.com/mrbiz/venture-os/internal/crm/builtin"
	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/genesis"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

var ctx = context.Background()

type env struct {
	signer *mcptoken.Signer
	url    string
	hub    *fakes.Hub
	llm    *fakes.LLM
	store  *store.PG
	pool   *pgxpool.Pool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.Pool(t)
	st := store.New(pool, testdb.Role)
	hub := fakes.NewHub()
	llm := fakes.NewLLM(hub)
	signer, _ := mcptoken.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	prov := &provisioner.Provisioner{
		Identity: fakes.NewIdentity(hub), ERP: fakes.NewERP(hub), CRM: fakes.NewCRM(hub), Support: fakes.NewSupport(hub),
		Runtime: fakes.NewRuntime(hub), Store: st, Tokens: signer,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	srv := &controlplane.Server{CRM: builtin.New(pool, tenancy.Options{AssumeRole: testdb.Role}), Repo: st, Interviewer: &genesis.Engine{LLM: llm, Model: "m"}, Provisioner: prov,
		AgentSigner: signer, Now: func() time.Time { return time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) }}
	ts := httptest.NewServer(srv.Handler(auth.StaticVerifier{
		"tok-founder": "sub-founder", "tok-other": "sub-other", "tok-viewer": "sub-viewer", "tok-approver": "sub-approver",
	}))
	t.Cleanup(ts.Close)
	return &env{url: ts.URL, hub: hub, llm: llm, store: st, signer: signer, pool: pool}
}

type clients struct {
	tenant tenantv1connect.TenantServiceClient
	gen    genesisv1connect.GenesisServiceClient
	prov   provisionerv1connect.ProvisionerServiceClient
}

func (e *env) as(token string) clients {
	opt := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			return next(c, req)
		}
	}))
	hc := http.DefaultClient
	return clients{
		tenant: tenantv1connect.NewTenantServiceClient(hc, e.url, opt),
		gen:    genesisv1connect.NewGenesisServiceClient(hc, e.url, opt),
		prov:   provisionerv1connect.NewProvisionerServiceClient(hc, e.url, opt),
	}
}

func envelope(t *testing.T, message string, bp *domain.Blueprint, ready bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"message": message, "blueprint": bp, "ready": ready})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func code(err error) connect.Code { return connect.CodeOf(err) }

func TestFounderJourneyFromIdeaToLiveBusiness(t *testing.T) {
	e := newEnv(t)
	f := e.as("tok-founder")
	bp := fixtures.EXIMBlueprint()

	// 1. Create the business and open the interview.
	cb, err := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "biz-1"}))
	if err != nil || !cb.Msg.Created {
		t.Fatalf("create business: %v", err)
	}
	tenant := cb.Msg.TenantId
	ss, err := f.gen.StartSession(ctx, connect.NewRequest(&genesisv1.StartSessionRequest{TenantId: tenant, IdempotencyKey: "s1"}))
	if err != nil {
		t.Fatal(err)
	}

	// 2. Two interview turns: a question, then a full draft.
	e.llm.Script(envelope(t, "Which country will you operate from?", nil, false), envelope(t, "Here is your plan. Shall I proceed?", &bp, true))
	r1, err := f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "I want to export spices", TurnKey: "t1"}))
	if err != nil || r1.Msg.Ready || len(r1.Msg.DraftBlueprintJson) != 0 {
		t.Fatalf("turn 1: %+v %v", r1, err)
	}
	r2, err := f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "Kerala to the EU, yes go", TurnKey: "t2"}))
	if err != nil || !r2.Msg.Ready || !r2.Msg.DraftChanged || len(r2.Msg.DraftBlueprintJson) == 0 {
		t.Fatalf("turn 2: %+v %v", r2, err)
	}

	// A retried turn is answered from storage without another model call.
	calls := len(e.llm.Requests())
	rr, err := f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "Kerala to the EU, yes go", TurnKey: "t2"}))
	if err != nil || !rr.Msg.Replayed || rr.Msg.AssistantMessage != r2.Msg.AssistantMessage {
		t.Fatalf("replayed turn: %+v %v", rr, err)
	}
	if len(e.llm.Requests()) != calls {
		t.Fatal("retried turn called the model again")
	}
	sess, err := f.gen.GetSession(ctx, connect.NewRequest(&genesisv1.GetSessionRequest{TenantId: tenant, SessionId: ss.Msg.SessionId}))
	if err != nil || len(sess.Msg.Turns) != 4 {
		t.Fatalf("transcript: %+v %v", sess, err)
	}

	// 3. Submit, review, approve.
	sub, err := f.gen.SubmitBlueprint(ctx, connect.NewRequest(&genesisv1.SubmitBlueprintRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, IdempotencyKey: "sub1"}))
	if err != nil || sub.Msg.Status != "DRAFT" || sub.Msg.Version != 1 {
		t.Fatalf("submit: %+v %v", sub, err)
	}
	if want, _ := bp.Hash(); sub.Msg.ContentHash != hex.EncodeToString(want[:]) {
		t.Fatal("content hash does not match the blueprint")
	}
	// Approving with a stale/incorrect hash is refused.
	_, err = f.gen.ApproveBlueprint(ctx, connect.NewRequest(&genesisv1.ApproveBlueprintRequest{TenantId: tenant, BlueprintId: sub.Msg.BlueprintId, ContentHash: strings.Repeat("0", 64)}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale hash: %v", err)
	}
	// Provisioning before approval is refused.
	_, err = f.prov.Provision(ctx, connect.NewRequest(&provisionerv1.ProvisionRequest{TenantId: tenant, BlueprintId: sub.Msg.BlueprintId, IdempotencyKey: "p0"}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("provision unapproved: %v", err)
	}
	if _, err := f.gen.ApproveBlueprint(ctx, connect.NewRequest(&genesisv1.ApproveBlueprintRequest{TenantId: tenant, BlueprintId: sub.Msg.BlueprintId, ContentHash: sub.Msg.ContentHash})); err != nil {
		t.Fatal(err)
	}

	// 4. Provision.
	pr, err := f.prov.Provision(ctx, connect.NewRequest(&provisionerv1.ProvisionRequest{TenantId: tenant, BlueprintId: sub.Msg.BlueprintId, IdempotencyKey: "p1"}))
	if err != nil {
		t.Fatal(err)
	}
	if pr.Msg.Status != "SUCCEEDED" || pr.Msg.Replayed || len(pr.Msg.Steps) != len(provisioner.StepKeys()) {
		t.Fatalf("provision: %+v", pr.Msg)
	}
	again, err := f.prov.Provision(ctx, connect.NewRequest(&provisionerv1.ProvisionRequest{TenantId: tenant, BlueprintId: sub.Msg.BlueprintId, IdempotencyKey: "p1"}))
	if err != nil || !again.Msg.Replayed || again.Msg.RunId != pr.Msg.RunId {
		t.Fatalf("replay: %+v %v", again, err)
	}
	st, err := f.prov.GetStatus(ctx, connect.NewRequest(&provisionerv1.GetStatusRequest{TenantId: tenant}))
	if err != nil || st.Msg.Status != "SUCCEEDED" || st.Msg.RunId != pr.Msg.RunId {
		t.Fatalf("status: %+v %v", st, err)
	}

	// 5. The portfolio shows an active business named from the blueprint.
	list, err := f.tenant.ListBusinesses(ctx, connect.NewRequest(&tenantv1.ListBusinessesRequest{}))
	if err != nil || len(list.Msg.Businesses) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	b := list.Msg.Businesses[0]
	if b.Status != "ACTIVE" || b.Slug != bp.Business.Slug || b.Name != bp.Business.Name || b.Role != "founder" || b.BaseCurrency != "INR" {
		t.Fatalf("business = %+v", b)
	}
}

func TestAuthenticationAndTenantIsolation(t *testing.T) {
	e := newEnv(t)
	f, other, anon := e.as("tok-founder"), e.as("tok-other"), e.as("")

	cb, err := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "k"}))
	if err != nil {
		t.Fatal(err)
	}
	tenant := cb.Msg.TenantId
	ss, _ := f.gen.StartSession(ctx, connect.NewRequest(&genesisv1.StartSessionRequest{TenantId: tenant, IdempotencyKey: "s"}))

	if _, err := anon.tenant.ListBusinesses(ctx, connect.NewRequest(&tenantv1.ListBusinessesRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous list: %v", err)
	}
	bad := e.as("forged")
	if _, err := bad.tenant.ListBusinesses(ctx, connect.NewRequest(&tenantv1.ListBusinessesRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("forged token: %v", err)
	}

	// Another authenticated user sees nothing and cannot act: NotFound everywhere, never PermissionDenied (no probing).
	if l, _ := other.tenant.ListBusinesses(ctx, connect.NewRequest(&tenantv1.ListBusinessesRequest{})); len(l.Msg.Businesses) != 0 {
		t.Error("another user lists the founder's business")
	}
	checks := map[string]error{}
	_, checks["start session"] = other.gen.StartSession(ctx, connect.NewRequest(&genesisv1.StartSessionRequest{TenantId: tenant, IdempotencyKey: "x"}))
	_, checks["converse"] = other.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "hi", TurnKey: "x"}))
	_, checks["get session"] = other.gen.GetSession(ctx, connect.NewRequest(&genesisv1.GetSessionRequest{TenantId: tenant, SessionId: ss.Msg.SessionId}))
	_, checks["provision"] = other.prov.Provision(ctx, connect.NewRequest(&provisionerv1.ProvisionRequest{TenantId: tenant, BlueprintId: ss.Msg.SessionId, IdempotencyKey: "x"}))
	_, checks["status"] = other.prov.GetStatus(ctx, connect.NewRequest(&provisionerv1.GetStatusRequest{TenantId: tenant}))
	for name, err := range checks {
		if code(err) != connect.CodeNotFound {
			t.Errorf("%s as outsider: code = %v (%v), want NotFound", name, code(err), err)
		}
	}
}

func TestViewerCanReadButNotAct(t *testing.T) {
	e := newEnv(t)
	f, v := e.as("tok-founder"), e.as("tok-viewer")
	cb, _ := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "k"}))
	tenant := cb.Msg.TenantId
	ss, _ := f.gen.StartSession(ctx, connect.NewRequest(&genesisv1.StartSessionRequest{TenantId: tenant, IdempotencyKey: "s"}))
	tid := mustUUID(t, tenant)
	if err := e.store.AddMember(ctx, tid, "sub-viewer", "viewer"); err != nil {
		t.Fatal(err)
	}

	if _, err := v.gen.GetSession(ctx, connect.NewRequest(&genesisv1.GetSessionRequest{TenantId: tenant, SessionId: ss.Msg.SessionId})); err != nil {
		t.Errorf("viewer read: %v", err)
	}
	if l, err := v.tenant.ListBusinesses(ctx, connect.NewRequest(&tenantv1.ListBusinessesRequest{})); err != nil || len(l.Msg.Businesses) != 1 || l.Msg.Businesses[0].Role != "viewer" {
		t.Errorf("viewer list: %+v %v", l, err)
	}
	_, err := v.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "hi", TurnKey: "x"}))
	if code(err) != connect.CodePermissionDenied {
		t.Errorf("viewer converse: %v", err)
	}
	_, err = v.gen.ApproveBlueprint(ctx, connect.NewRequest(&genesisv1.ApproveBlueprintRequest{TenantId: tenant, BlueprintId: ss.Msg.SessionId}))
	if code(err) != connect.CodePermissionDenied {
		t.Errorf("viewer approve: %v", err)
	}
	_, err = v.prov.Provision(ctx, connect.NewRequest(&provisionerv1.ProvisionRequest{TenantId: tenant, BlueprintId: ss.Msg.SessionId, IdempotencyKey: "x"}))
	if code(err) != connect.CodePermissionDenied {
		t.Errorf("viewer provision: %v", err)
	}
}

func TestInputValidation(t *testing.T) {
	e := newEnv(t)
	f := e.as("tok-founder")
	cb, _ := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "k"}))
	tenant := cb.Msg.TenantId
	ss, _ := f.gen.StartSession(ctx, connect.NewRequest(&genesisv1.StartSessionRequest{TenantId: tenant, IdempotencyKey: "s"}))

	cases := map[string]error{}
	_, cases["empty key"] = f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{}))
	_, cases["bad tenant id"] = f.gen.StartSession(ctx, connect.NewRequest(&genesisv1.StartSessionRequest{TenantId: "nope", IdempotencyKey: "x"}))
	_, cases["empty message"] = f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "  ", TurnKey: "x"}))
	_, cases["huge message"] = f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: strings.Repeat("x", genesis.MaxFounderMessageBytes+1), TurnKey: "x"}))
	_, cases["no turn key"] = f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "hi"}))
	_, cases["bad session id"] = f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: "x", Message: "hi", TurnKey: "x"}))
	for name, err := range cases {
		if code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: code = %v (%v), want InvalidArgument", name, code(err), err)
		}
	}
	// Submitting before any valid draft exists is a precondition failure, not a crash.
	_, err := f.gen.SubmitBlueprint(ctx, connect.NewRequest(&genesisv1.SubmitBlueprintRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, IdempotencyKey: "x"}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Errorf("submit without draft: %v", err)
	}
}

func TestModelOutageIsReportedAsUnavailableAndRecordsNothing(t *testing.T) {
	e := newEnv(t)
	f := e.as("tok-founder")
	cb, _ := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "k"}))
	tenant := cb.Msg.TenantId
	ss, _ := f.gen.StartSession(ctx, connect.NewRequest(&genesisv1.StartSessionRequest{TenantId: tenant, IdempotencyKey: "s"}))

	e.hub.Fail("llm", "Complete", transient(), 1)
	_, err := f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "hi", TurnKey: "t1"}))
	if code(err) != connect.CodeUnavailable {
		t.Fatalf("outage: %v", err)
	}
	got, _ := f.gen.GetSession(ctx, connect.NewRequest(&genesisv1.GetSessionRequest{TenantId: tenant, SessionId: ss.Msg.SessionId}))
	if len(got.Msg.Turns) != 0 {
		t.Fatalf("a failed turn left %d messages behind", len(got.Msg.Turns))
	}
	// The same turn key can simply be retried.
	e.llm.Script(envelope(t, "Back online. What do you sell?", nil, false))
	if _, err := f.gen.Converse(ctx, connect.NewRequest(&genesisv1.ConverseRequest{TenantId: tenant, SessionId: ss.Msg.SessionId, Message: "hi", TurnKey: "t1"})); err != nil {
		t.Fatalf("retry after outage: %v", err)
	}
}

func TestFailedProvisioningIsReportedAndVisibleInStatus(t *testing.T) {
	e := newEnv(t)
	f := e.as("tok-founder")
	bp := fixtures.EXIMBlueprint()
	cb, _ := f.tenant.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "k"}))
	tenant := cb.Msg.TenantId
	tid := mustUUID(t, tenant)
	sb, err := e.store.SaveBlueprint(ctx, tid, nil, bp, "sub-founder", "bp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ApproveBlueprint(ctx, tid, sb.ID, "sub-founder"); err != nil {
		t.Fatal(err)
	}

	e.hub.Fail("erp", "EnsureCompany", errPermanent(), -1)
	_, err = f.prov.Provision(ctx, connect.NewRequest(&provisionerv1.ProvisionRequest{TenantId: tenant, BlueprintId: sb.ID.String(), IdempotencyKey: "p1"}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "erp.company") || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("failure: %v", err)
	}
	if strings.Contains(err.Error(), "erpnext exploded") {
		t.Error("internal error text leaked to the client")
	}
	st, err := f.prov.GetStatus(ctx, connect.NewRequest(&provisionerv1.GetStatusRequest{TenantId: tenant}))
	if err != nil || st.Msg.Status != "ROLLED_BACK" {
		t.Fatalf("status: %+v %v", st, err)
	}
	failed := false
	for _, s := range st.Msg.Steps {
		if s.Key == "erp.company" && s.Status == "COMPENSATED" || s.Key == "erp.company" && s.Status == "FAILED" {
			failed = true
		}
	}
	if !failed {
		t.Errorf("status does not show the failed step: %+v", st.Msg.Steps)
	}
	l, _ := f.tenant.ListBusinesses(ctx, connect.NewRequest(&tenantv1.ListBusinessesRequest{}))
	if l.Msg.Businesses[0].Status != "DRAFT" {
		t.Errorf("tenant status after failure = %s", l.Msg.Businesses[0].Status)
	}
}
