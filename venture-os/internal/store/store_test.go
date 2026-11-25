package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/venture-os/internal/connectors/hermes"
	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

var ctx = context.Background()

func newStore(t *testing.T) (*store.PG, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.Pool(t)
	return store.New(pool, testdb.Role), pool
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// approvedTenant creates a tenant with an approved EXIM blueprint.
func approvedTenant(t *testing.T, s *store.PG, founder string) (uuid.UUID, store.StoredBlueprint) {
	t.Helper()
	tn, _, err := s.CreateTenant(ctx, founder, "create-"+founder)
	must(t, err)
	sb, err := s.SaveBlueprint(ctx, tn, nil, fixtures.EXIMBlueprint(), founder, "bp-"+founder)
	must(t, err)
	sb, err = s.ApproveBlueprint(ctx, tn, sb.ID, founder)
	must(t, err)
	return tn, sb
}

// scalar runs a read-only query as the tenant (through RLS).
func scalar[T any](t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, sql string, args ...any) T {
	t.Helper()
	var v T
	err := tenancy.WithScopeTx(ctx, pool, tenancy.Scope{TenantID: tenant}, tenancy.Options{AssumeRole: testdb.Role}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&v)
	})
	must(t, err)
	return v
}

func TestTenantCreationIsIdempotentAndIsolated(t *testing.T) {
	s, _ := newStore(t)
	a1, created, err := s.CreateTenant(ctx, "user-a", "k1")
	must(t, err)
	if !created {
		t.Fatal("first create should report created")
	}
	a2, created, err := s.CreateTenant(ctx, "user-a", "k1")
	must(t, err)
	if created || a2 != a1 {
		t.Fatalf("replay: created=%v id=%s want %s", created, a2, a1)
	}
	b, _, err := s.CreateTenant(ctx, "user-b", "k1") // same key, different founder: independent
	must(t, err)
	if b == a1 {
		t.Fatal("different founders share a tenant")
	}

	ms, err := s.Memberships(ctx, "user-a")
	must(t, err)
	if len(ms) != 1 || ms[0].Tenant.ID != a1 || ms[0].Role != "founder" || ms[0].Tenant.Status != "DRAFT" {
		t.Fatalf("memberships = %+v", ms)
	}
	if _, err := s.Tenant(ctx, a1); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.Memberships(ctx, "nobody"); len(ms) != 0 {
		t.Fatal("unknown user has memberships")
	}
}

func TestGenesisTurnsAreRecordedOnceAndDraftPersists(t *testing.T) {
	s, _ := newStore(t)
	tn, _, err := s.CreateTenant(ctx, "u", "k")
	must(t, err)
	sess, err := s.StartGenesisSession(ctx, tn, "u", "s1")
	must(t, err)
	if again, _ := s.StartGenesisSession(ctx, tn, "u", "s1"); again != sess {
		t.Fatal("session start is not idempotent")
	}

	bp := fixtures.EXIMBlueprint()
	replayed, err := s.RecordTurn(ctx, tn, sess, "turn-1", "I export spices", "Where from?", nil)
	must(t, err)
	if replayed {
		t.Fatal("first turn reported as replay")
	}
	replayed, err = s.RecordTurn(ctx, tn, sess, "turn-2", "Kerala", "Great, here's a plan", &bp)
	must(t, err)
	// A client retry of turn-2 must not duplicate messages or clobber the draft.
	replayed, err = s.RecordTurn(ctx, tn, sess, "turn-2", "Kerala", "DIFFERENT", nil)
	must(t, err)
	if !replayed {
		t.Fatal("retry not detected")
	}

	msgs, draft, status, err := s.GenesisState(ctx, tn, sess)
	must(t, err)
	if status != "ACTIVE" || len(msgs) != 4 || msgs[3].Content != "Great, here's a plan" {
		t.Fatalf("state: status=%s msgs=%+v", status, msgs)
	}
	if draft == nil || draft.Business.Slug != bp.Business.Slug {
		t.Fatalf("draft not persisted: %+v", draft)
	}
}

func TestBlueprintLifecycle(t *testing.T) {
	s, _ := newStore(t)
	tn, _, err := s.CreateTenant(ctx, "u", "k")
	must(t, err)
	bp := fixtures.EXIMBlueprint()

	v1, err := s.SaveBlueprint(ctx, tn, nil, bp, "u", "bp-1")
	must(t, err)
	again, err := s.SaveBlueprint(ctx, tn, nil, bp, "u", "bp-1")
	must(t, err)
	if again.ID != v1.ID || v1.Version != 1 {
		t.Fatalf("replay: %+v vs %+v", again, v1)
	}
	if want, _ := bp.Hash(); v1.Hash != want {
		t.Fatal("hash changed across the jsonb round trip")
	}

	bad := bp
	bad.Pricing = nil
	if _, err := s.SaveBlueprint(ctx, tn, nil, bad, "u", "bp-bad"); !domain.IsValidationError(err) {
		t.Fatalf("invalid blueprint stored: %v", err)
	}

	approved, err := s.ApproveBlueprint(ctx, tn, v1.ID, "founder-sub")
	must(t, err)
	if approved.Status != "APPROVED" || approved.ApprovedBy != "founder-sub" {
		t.Fatalf("approved = %+v", approved)
	}
	org, _ := s.Tenant(ctx, tn)
	if org.Slug != bp.Business.Slug || org.Name != bp.Business.Name || org.BaseCurrency != "INR" || org.Country != "IN" {
		t.Fatalf("org identity not applied from blueprint: %+v", org)
	}
	if again, err := s.ApproveBlueprint(ctx, tn, v1.ID, "founder-sub"); err != nil || again.Status != "APPROVED" {
		t.Fatalf("re-approve should be a no-op: %v", err)
	}

	// v2 supersedes v1.
	bp2 := bp
	bp2.Business.Description = "Now also exporting turmeric"
	v2, err := s.SaveBlueprint(ctx, tn, nil, bp2, "u", "bp-2")
	must(t, err)
	if v2.Version != 2 {
		t.Fatalf("version = %d", v2.Version)
	}
	if _, err := s.ApproveBlueprint(ctx, tn, v2.ID, "founder-sub"); err != nil {
		t.Fatal(err)
	}
	old, _ := s.Blueprint(ctx, tn, v1.ID)
	if old.Status != "SUPERSEDED" {
		t.Fatalf("v1 status = %s", old.Status)
	}
	latest, _ := s.LatestBlueprint(ctx, tn)
	if latest.ID != v2.ID {
		t.Fatal("latest blueprint wrong")
	}
}

func TestSlugCollisionIsReportedAndTenantsCannotReadEachOthersBlueprints(t *testing.T) {
	s, _ := newStore(t)
	a, aBP := approvedTenant(t, s, "user-a")

	b, _, err := s.CreateTenant(ctx, "user-b", "k")
	must(t, err)
	v, err := s.SaveBlueprint(ctx, b, nil, fixtures.EXIMBlueprint(), "user-b", "bp") // same slug as tenant a
	must(t, err)
	if _, err := s.ApproveBlueprint(ctx, b, v.ID, "user-b"); !errors.Is(err, store.ErrSlugTaken) {
		t.Fatalf("slug collision: %v", err)
	}
	if _, err := s.Blueprint(ctx, b, aBP.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tenant b read tenant a's blueprint: %v", err)
	}
	_ = a
}

type harness struct {
	p     *provisioner.Provisioner
	hub   *fakes.Hub
	erp   *fakes.ERP
	pool  *pgxpool.Pool
	store *store.PG
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, pool := newStore(t)
	hub := fakes.NewHub()
	signer, _ := mcptoken.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	erp := fakes.NewERP(hub)
	return &harness{
		hub: hub, erp: erp, pool: pool, store: s,
		p: &provisioner.Provisioner{
			Identity: fakes.NewIdentity(hub), ERP: erp, CRM: fakes.NewCRM(hub), Support: fakes.NewSupport(hub),
			Runtime: fakes.NewRuntime(hub), Store: s, Tokens: signer,
			Sleep: func(context.Context, time.Duration) error { return nil },
		},
	}
}

func (h *harness) request(tn uuid.UUID, sb store.StoredBlueprint, key string) provisioner.Request {
	info, _ := h.store.Tenant(ctx, tn)
	return provisioner.Request{Tenant: info.Info(), FounderSub: "founder", BlueprintID: sb.ID, Blueprint: sb.Blueprint, IdempotencyKey: key}
}

func TestProvisioningThroughPostgresSeedsTheTenant(t *testing.T) {
	h := newHarness(t)
	tn, sb := approvedTenant(t, h.store, "founder")
	res, err := h.p.Provision(ctx, h.request(tn, sb, "prov-1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != provisioner.RunSucceeded {
		t.Fatalf("result = %+v", res)
	}

	org, _ := h.store.Tenant(ctx, tn)
	if org.Status != "ACTIVE" {
		t.Errorf("tenant status = %s", org.Status)
	}
	checks := []struct {
		name string
		sql  string
		want int
	}{
		{"active accounts", `SELECT count(*) FROM ledger_accounts WHERE active`, 11},
		{"accounts with a parent", `SELECT count(*) FROM ledger_accounts WHERE parent_id IS NOT NULL`, 5},
		{"active pricing rules", `SELECT count(*) FROM tenant_pricing_rules WHERE status='ACTIVE'`, 1},
		{"pipelines", `SELECT count(*) FROM crm_pipelines WHERE status='ACTIVE'`, 2},
		{"workflows", `SELECT count(*) FROM workflows WHERE status='ACTIVE'`, 1},
		{"agents", `SELECT count(*) FROM agent_org_configs`, 5},
		{"approved skills", `SELECT count(*) FROM agent_org_configs WHERE skill_status='APPROVED' AND length(skill_sha256)=32`, 5},
		{"integrations", `SELECT count(*) FROM tenant_integrations WHERE status='ACTIVE'`, 5},
		{"succeeded steps", `SELECT count(*) FROM provision_steps WHERE status='SUCCEEDED'`, len(provisioner.StepKeys())},
		{"audit entries", `SELECT count(*) FROM audit_logs WHERE action LIKE 'provision.%'`, 2},
	}
	for _, c := range checks {
		if got := scalar[int](t, h.pool, tn, c.sql); got != c.want {
			t.Errorf("%s = %d, want %d", c.name, got, c.want)
		}
	}
	if st := scalar[string](t, h.pool, tn, `SELECT status FROM provision_runs`); st != "SUCCEEDED" {
		t.Errorf("run status = %s", st)
	}
	if rep, _ := scalar[string](t, h.pool, tn, `SELECT reports_to_agent_key FROM agent_org_configs WHERE agent_key='support'`), 0; rep != "sales" {
		t.Errorf("org chart edge lost: support reports to %q", rep)
	}

	// Replay: same answer, no new external calls.
	calls := h.hub.ExternalCalls()
	again, err := h.p.Provision(ctx, h.request(tn, sb, "prov-1"))
	if err != nil || !again.Replayed || again.RunID != res.RunID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if h.hub.ExternalCalls() != calls {
		t.Fatal("replay touched external systems")
	}
	run, err := h.store.LatestRun(ctx, tn)
	must(t, err)
	if run.Run.ID != res.RunID || len(run.Steps) != len(provisioner.StepKeys()) {
		t.Fatalf("latest run = %+v", run)
	}
}

func TestFailedProvisioningRollsBackPostgresAndCanBeRetried(t *testing.T) {
	h := newHarness(t)
	tn, sb := approvedTenant(t, h.store, "founder")

	h.hub.Fail("support", "EnsureInboxes", errors.New("libredesk down"), 1)
	_, err := h.p.Provision(ctx, h.request(tn, sb, "prov-1"))
	var pe *provisioner.Error
	if !errors.As(err, &pe) || pe.Status != provisioner.RunRolledBack {
		t.Fatalf("err = %v", err)
	}
	org, _ := h.store.Tenant(ctx, tn)
	if org.Status != "DRAFT" {
		t.Errorf("tenant status after rollback = %s", org.Status)
	}
	for name, c := range map[string]struct {
		sql  string
		want int
	}{
		"pipelines":       {`SELECT count(*) FROM crm_pipelines`, 0},
		"agents":          {`SELECT count(*) FROM agent_org_configs`, 0},
		"integrations":    {`SELECT count(*) FROM tenant_integrations`, 0},
		"active accounts": {`SELECT count(*) FROM ledger_accounts WHERE active`, 0},
		"active pricing":  {`SELECT count(*) FROM tenant_pricing_rules WHERE status='ACTIVE'`, 0},
		"active workflow": {`SELECT count(*) FROM workflows WHERE status='ACTIVE'`, 0},
	} {
		if got := scalar[int](t, h.pool, tn, c.sql); got != c.want {
			t.Errorf("%s after rollback = %d, want %d", name, got, c.want)
		}
	}
	if st := scalar[string](t, h.pool, tn, `SELECT status FROM provision_runs WHERE idempotency_key='prov-1'`); st != "ROLLED_BACK" {
		t.Errorf("run status = %s", st)
	}

	// Retry with a new key: seeds are revived, not duplicated, and the tenant goes live.
	if _, err := h.p.Provision(ctx, h.request(tn, sb, "prov-2")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := scalar[int](t, h.pool, tn, `SELECT count(*) FROM ledger_accounts`); n != 11 {
		t.Errorf("accounts after retry = %d, want 11 (no duplicates)", n)
	}
	if n := scalar[int](t, h.pool, tn, `SELECT count(*) FROM ledger_accounts WHERE active`); n != 11 {
		t.Errorf("active accounts after retry = %d", n)
	}
	if org, _ := h.store.Tenant(ctx, tn); org.Status != "ACTIVE" {
		t.Errorf("status = %s", org.Status)
	}
}

func TestSeedingRefusesAnythingButTheApprovedBlueprint(t *testing.T) {
	h := newHarness(t)
	tn, sb := approvedTenant(t, h.store, "founder")

	// A different blueprint than the one approved (e.g. tampered in memory) is refused.
	tampered := sb.Blueprint
	tampered.Pricing = append([]domain.PricingRule(nil), tampered.Pricing...)
	tampered.Pricing[0].MarginFloorBps = 1
	if err := h.store.SeedTenant(ctx, tn, sb.ID, tampered, nil); err == nil {
		t.Fatal("tampered blueprint was seeded")
	}

	// An unapproved blueprint cannot even start a run.
	draft, err := h.store.SaveBlueprint(ctx, tn, nil, func() domain.Blueprint {
		b := sb.Blueprint
		b.Business.Description = "v2 draft"
		return b
	}(), "founder", "bp-v2")
	must(t, err)
	_, _, err = h.store.BeginRun(ctx, tn, draft.ID, "k", time.Minute)
	if !errors.Is(err, store.ErrNotApproved) {
		t.Fatalf("draft blueprint run: %v", err)
	}

	// A skill whose text does not match its hash is refused.
	if err := h.store.SeedTenant(ctx, tn, sb.ID, sb.Blueprint, []ports.Skill{{AgentKey: "ceo", Markdown: "x"}}); err == nil {
		t.Fatal("skill with wrong hash accepted")
	}
}

func TestRunClaimingSemantics(t *testing.T) {
	h := newHarness(t)
	tn, sb := approvedTenant(t, h.store, "founder")

	run, claimed, err := h.store.BeginRun(ctx, tn, sb.ID, "k1", time.Hour)
	must(t, err)
	if !claimed || run.Status != provisioner.RunPending {
		t.Fatalf("first claim: %+v claimed=%v", run, claimed)
	}
	same, claimed, err := h.store.BeginRun(ctx, tn, sb.ID, "k1", time.Hour)
	must(t, err)
	if claimed || same.ID != run.ID {
		t.Fatal("a live run inside its lease must not be re-claimed")
	}
	if _, _, err := h.store.BeginRun(ctx, tn, sb.ID, "k2", time.Hour); !errors.Is(err, store.ErrTenantBusy) {
		t.Fatalf("second active run for the tenant: %v", err)
	}
	took, claimed, err := h.store.BeginRun(ctx, tn, sb.ID, "k1", 0) // zero lease == expired
	must(t, err)
	if !claimed || took.ID != run.ID {
		t.Fatal("an expired lease must be taken over")
	}
	must(t, h.store.FinishRun(ctx, tn, run.ID, provisioner.RunRolledBack, "x"))
	done, claimed, err := h.store.BeginRun(ctx, tn, sb.ID, "k1", 0)
	must(t, err)
	if claimed || done.Status != provisioner.RunRolledBack {
		t.Fatal("a terminal run must never be re-claimed")
	}
	if _, claimed, err := h.store.BeginRun(ctx, tn, sb.ID, "k2", time.Hour); err != nil || !claimed {
		t.Fatalf("a new key after the old run ended: claimed=%v err=%v", claimed, err)
	}
}

func TestEnsureRestrictedRoleRejectsSuperuser(t *testing.T) {
	_, pool := newStore(t) // the test pool connects as a superuser
	if err := store.EnsureRestrictedRole(ctx, pool); err == nil {
		t.Fatal("a superuser connection must be rejected")
	}
	conn, err := pool.Acquire(ctx)
	must(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, "SET ROLE venture_app")
	must(t, err)
	var unrestricted bool
	must(t, conn.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&unrestricted))
	if unrestricted {
		t.Fatal("venture_app must be restricted")
	}
}

func TestProvisioningWithTheRealHermesAdapterWritesATenantBoundProfileAndRollsBack(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.p.Runtime = &hermes.Runtime{Root: dir, MCPURL: "http://mcpbus:8090/mcp"}
	tn, sb := approvedTenant(t, h.store, "founder")
	profile := filepath.Join(dir, "profiles", "venture-"+sb.Blueprint.Business.Slug)

	// A later step fails: the real profile written in stage 2 must be removed again.
	h.hub.Fail("crm", "ApplyPipelines", errors.New("crm down"), 1)
	if _, err := h.p.Provision(ctx, h.request(tn, sb, "prov-1")); err == nil {
		t.Fatal("want failure")
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("rollback left the Hermes profile behind (%v)", err)
	}

	// Retry succeeds and writes the real layout.
	if _, err := h.p.Provision(ctx, h.request(tn, sb, "prov-2")); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(profile, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(strings.TrimPrefix(string(env), "VENTURE_MCP_TOKEN="))
	signer, _ := mcptoken.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	claims, err := signer.Verify(token)
	if err != nil || claims.TenantID != tn {
		t.Fatalf("the profile token is not bound to this tenant: %+v %v", claims, err)
	}
	if cfg, _ := os.ReadFile(filepath.Join(profile, "config.yaml")); strings.Contains(string(cfg), token) {
		t.Fatal("token leaked into config.yaml")
	}

	// Every skill on disk is byte-for-byte the one recorded (and approved) in Postgres.
	rows := scalar[int](t, h.pool, tn, `SELECT count(*) FROM agent_org_configs WHERE skill_status='APPROVED'`)
	if rows != 5 {
		t.Fatalf("approved skills in db = %d", rows)
	}
	for _, key := range []string{"ceo", "sales", "sourcing", "finance", "support"} {
		onDisk, err := os.ReadFile(filepath.Join(profile, "skills", "venture", key, "SKILL.md"))
		if err != nil {
			t.Fatalf("skill %s not installed: %v", key, err)
		}
		sum := sha256.Sum256(onDisk)
		var dbSum []byte
		must(t, tenancy.WithScopeTx(ctx, h.pool, tenancy.Scope{TenantID: tn}, tenancy.Options{AssumeRole: testdb.Role}, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT skill_sha256 FROM agent_org_configs WHERE agent_key=$1`, key).Scan(&dbSum)
		}))
		if string(dbSum) != string(sum[:]) {
			t.Errorf("skill %s on disk differs from the approved hash in Postgres", key)
		}
	}
}
