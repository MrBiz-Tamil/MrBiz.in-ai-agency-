// Package testenv assembles a complete in-process Venture OS control plane for end-to-end tests: real Postgres
// (PGlite or $VENTURE_TEST_DATABASE_URL) with the schema applied, the real ConnectRPC services and provisioner,
// fake external systems, and a static OIDC verifier. Only tests import it.
package testenv

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	approvalv1 "github.com/mrbiz/venture-os/gen/venture/approval/v1"
	"github.com/mrbiz/venture-os/gen/venture/approval/v1/approvalv1connect"
	provisionerv1 "github.com/mrbiz/venture-os/gen/venture/provisioner/v1"
	"github.com/mrbiz/venture-os/gen/venture/provisioner/v1/provisionerv1connect"
	tenantv1 "github.com/mrbiz/venture-os/gen/venture/tenant/v1"
	"github.com/mrbiz/venture-os/gen/venture/tenant/v1/tenantv1connect"
	"github.com/mrbiz/venture-os/internal/auth"
	"github.com/mrbiz/venture-os/internal/collabsvc"
	"github.com/mrbiz/venture-os/internal/connectors/fakeapi"
	"github.com/mrbiz/venture-os/internal/controlplane"
	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/crm/builtin"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/genesis"
	"github.com/mrbiz/venture-os/internal/httpx"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
	"github.com/mrbiz/venture-os/internal/vault"
)

// Stack is a running control plane.
type Stack struct {
	URL    string
	Store  *store.PG
	Signer *mcptoken.Signer
	Hub    *fakes.Hub
	Pool   *pgxpool.Pool
	// CRM is the built-in CRM the control plane serves crm_ops from; nil with WithoutCRM.
	CRM crm.Provider
	// With WithCollab: the fake providers behind the integrations, and the credentials they accept.
	Google *fakeapi.Google
	Zoom   *fakeapi.Zoom
	Notion *fakeapi.Notion
	Collab *collabsvc.Registry
}

// Credentials the fake providers accept (see WithCollab).
var (
	GoogleCreds = map[string]string{"client_id": "gid", "client_secret": "gsecret", "refresh_token": "grefresh"}
	ZoomCreds   = map[string]string{"account_id": "acct", "client_id": "zid", "client_secret": "zsecret"}
	NotionCreds = map[string]string{"token": "ntok"}
	NotionDB    = "a1b2c3d4-0000-4000-8000-000000000001"
)

// Option adjusts New.
type Option func(*options)

type options struct{ noCRM, collab bool }

// WithCollab enables integrations backed by in-process fakes of Google Calendar, Zoom and Notion. Connect with
// GoogleCreds, ZoomCreds and NotionCreds (Notion also needs {"database_id": NotionDB}).
func WithCollab() Option { return func(o *options) { o.collab = true } }

// WithoutCRM builds a control plane with no CRM connected, as when the CRM connector is simulated.
func WithoutCRM() Option { return func(o *options) { o.noCRM = true } }

// OIDC tokens the stack accepts, mapped to subjects.
var Tokens = auth.StaticVerifier{"tok-founder": "sub-founder", "tok-other": "sub-other"}

// New starts a stack. The fixed clock is 2026-03-01 09:00 UTC.
func New(t *testing.T, opts ...Option) *Stack {
	t.Helper()
	var o options
	for _, f := range opts {
		f(&o)
	}
	pool := testdb.Pool(t)
	st := store.New(pool, testdb.Role)
	hub := fakes.NewHub()
	signer, err := mcptoken.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	prov := &provisioner.Provisioner{
		Identity: fakes.NewIdentity(hub), ERP: fakes.NewERP(hub), CRM: fakes.NewCRM(hub), Support: fakes.NewSupport(hub),
		Runtime: fakes.NewRuntime(hub), Store: st, Tokens: signer,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	var crmProvider crm.Provider
	if !o.noCRM {
		crmProvider = builtin.New(pool, tenancy.Options{AssumeRole: testdb.Role})
	}
	var collabReg *collabsvc.Registry
	var g *fakeapi.Google
	var z *fakeapi.Zoom
	var n *fakeapi.Notion
	if o.collab {
		g, z, n = fakeapi.NewGoogle("gid", "gsecret", "grefresh"), fakeapi.NewZoom("acct", "zid", "zsecret"), fakeapi.NewNotion("ntok", NotionDB, "Name", "Status")
		t.Cleanup(func() { g.Close(); z.Close(); n.Close() })
		v, err := vault.New(map[int][]byte{1: bytes.Repeat([]byte{5}, 32)}, 1)
		if err != nil {
			t.Fatal(err)
		}
		fast := func(h *httpx.Config) { h.Sleep = func(context.Context, time.Duration) error { return nil } }
		collabReg = &collabsvc.Registry{Store: st, Vault: v, Endpoints: collabsvc.Endpoints{
			GoogleAPI: g.URL(), GoogleOAuth: g.URL(), ZoomAPI: z.URL(), ZoomOAuth: z.URL(), NotionAPI: n.URL() + "/v1", Tune: fast,
			Now: func() time.Time { return time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) }}}
		g.Now, z.Now = func() time.Time { return time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) }, func() time.Time { return time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) }
	}
	srv := &controlplane.Server{
		Collab: collabReg,
		CRM:    crmProvider,
		Repo:   st, Interviewer: &genesis.Engine{LLM: fakes.NewLLM(hub), Model: "m"}, Provisioner: prov, AgentSigner: signer,
		Now: func() time.Time { return time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) },
	}
	ts := httptest.NewServer(srv.Handler(Tokens))
	t.Cleanup(ts.Close)
	return &Stack{URL: ts.URL, Store: st, Signer: signer, Hub: hub, Pool: pool, CRM: crmProvider, Google: g, Zoom: z, Notion: n, Collab: collabReg}
}

func bearer(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(c, req)
		}
	})
}

// Approvals returns a client for the founder's approval inbox.
func (s *Stack) Approvals(oidcToken string) approvalv1connect.ApprovalServiceClient {
	return approvalv1connect.NewApprovalServiceClient(http.DefaultClient, s.URL, connect.WithInterceptors(bearer(oidcToken)))
}

// Business is a provisioned, ACTIVE tenant.
type Business struct {
	ID         string
	UUID       uuid.UUID
	AgentToken string
}

// LiveBusiness creates a business for the founder behind oidcToken, approves the EXIM fixture under slug,
// provisions it, and funds the bank account with INR 5,000,000.00.
func (s *Stack) LiveBusiness(t *testing.T, oidcToken, slug string) Business {
	t.Helper()
	ctx := context.Background()
	opt := connect.WithInterceptors(bearer(oidcToken))
	tenants := tenantv1connect.NewTenantServiceClient(http.DefaultClient, s.URL, opt)
	prov := provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, s.URL, opt)

	cb, err := tenants.CreateBusiness(ctx, connect.NewRequest(&tenantv1.CreateBusinessRequest{IdempotencyKey: "biz-" + slug}))
	if err != nil {
		t.Fatal(err)
	}
	tid := uuid.MustParse(cb.Msg.TenantId)
	bp := fixtures.EXIMBlueprint()
	bp.Business.Slug, bp.Business.Name = slug, slug
	sb, err := s.Store.SaveBlueprint(ctx, tid, nil, bp, "seed", "bp-"+slug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.ApproveBlueprint(ctx, tid, sb.ID, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Provision(ctx, connect.NewRequest(&provisionerv1.ProvisionRequest{
		TenantId: cb.Msg.TenantId, BlueprintId: sb.ID.String(), IdempotencyKey: "prov-" + slug})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Store.PostJournal(ctx, tid, store.Journal{
		Memo: "opening capital", CreatedBy: "seed", IdempotencyKey: "capital",
		Entries: []store.LedgerEntry{
			{AccountCode: "1010", Debit: 500000000, Currency: "INR"},
			{AccountCode: "3000", Credit: 500000000, Currency: "INR"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tok, err := s.Signer.Mint(tid, "sales", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return Business{ID: cb.Msg.TenantId, UUID: tid, AgentToken: tok}
}

// Decide approves (with the correct payload hash) or rejects an approval as the founder behind oidcToken.
func (s *Stack) Decide(t *testing.T, oidcToken, tenant, approvalID string, approve bool) *approvalv1.DecideApprovalResponse {
	t.Helper()
	ctx := context.Background()
	c := s.Approvals(oidcToken)
	g, err := c.GetApproval(ctx, connect.NewRequest(&approvalv1.GetApprovalRequest{TenantId: tenant, ApprovalId: approvalID}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{
		TenantId: tenant, ApprovalId: approvalID, Approve: approve, PayloadHash: g.Msg.PayloadHash}))
	if err != nil {
		t.Fatal(err)
	}
	return r.Msg
}
