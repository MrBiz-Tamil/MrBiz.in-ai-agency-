package provisioner_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/provisioner"
)

var signingKey = []byte("0123456789abcdef0123456789abcdef")

type world struct {
	hub    *fakes.Hub
	id     *fakes.Identity
	erp    *fakes.ERP
	crm    *fakes.CRM
	sup    *fakes.Support
	rt     *fakes.Runtime
	store  *fakes.MemStore
	signer *mcptoken.Signer
	p      *provisioner.Provisioner
}

func newWorld(t *testing.T) *world {
	t.Helper()
	hub := fakes.NewHub()
	signer, err := mcptoken.NewSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{
		hub: hub, id: fakes.NewIdentity(hub), erp: fakes.NewERP(hub), crm: fakes.NewCRM(hub),
		sup: fakes.NewSupport(hub), rt: fakes.NewRuntime(hub),
		store: fakes.NewMemStore(hub), signer: signer,
	}
	w.p = &provisioner.Provisioner{
		Identity: w.id, ERP: w.erp, CRM: w.crm, Support: w.sup, Runtime: w.rt,
		Store: w.store, Tokens: signer,
		Sleep: func(context.Context, time.Duration) error { return nil }, // no real backoff in tests
	}
	return w
}

func (w *world) request(key string) provisioner.Request {
	bp := fixtures.EXIMBlueprint()
	return provisioner.Request{
		Tenant:         ports.TenantInfo{ID: uuid.New(), Slug: bp.Business.Slug, Name: bp.Business.Name},
		FounderSub:     "founder-1",
		BlueprintID:    uuid.New(),
		Blueprint:      bp,
		IdempotencyKey: key,
	}
}

func (w *world) exists(tenant uuid.UUID) map[string]bool {
	return map[string]bool{
		"identity": w.id.HasOrganization(tenant),
		"erp":      func() bool { _, ok := w.erp.Company(tenant); return ok }(),
		"crm":      w.crm.HasWorkspace(tenant),
		"support":  w.sup.HasInboxes(tenant),
		"runtime":  w.rt.HasProfile(tenant),
	}
}

func TestProvisionsEverySystem(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	res, err := w.p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	tn := req.Tenant.ID
	if res.Status != provisioner.RunSucceeded || res.Replayed {
		t.Fatalf("result = %+v", res)
	}
	for sys, ok := range w.exists(tn) {
		if !ok {
			t.Errorf("%s not provisioned", sys)
		}
	}
	if got := w.store.TenantStatus(tn); got != "ACTIVE" {
		t.Errorf("tenant status = %s", got)
	}
	if h := w.store.StatusHistory(tn); strings.Join(h, ",") != "PROVISIONING,ACTIVE" {
		t.Errorf("status history = %v", h)
	}
	if !w.store.Seeded(tn) {
		t.Error("database not seeded")
	}
	ints := w.store.Integrations(tn)
	for _, sys := range []string{"keycloak", "erpnext", "twenty", "libredesk", "hermes"} {
		if ints[sys] == "" {
			t.Errorf("integration %s not recorded", sys)
		}
	}
	co, _ := w.erp.Company(tn)
	if len(co.Accounts) != len(req.Blueprint.Accounting.ChartOfAccounts) {
		t.Errorf("erp accounts = %d", len(co.Accounts))
	}
	if n := len(w.crm.Pipelines(tn)); n != 2 {
		t.Errorf("pipelines = %d", n)
	}
	if n := len(w.sup.KBDocs(tn)); n != 1 {
		t.Errorf("kb docs = %d", n)
	}
	if n := len(w.rt.Skills(tn)); n != len(req.Blueprint.Agents) {
		t.Errorf("skills installed = %d", n)
	}
	// The Hermes profile is bound to this tenant and nobody else.
	claims, err := w.signer.Verify(w.rt.TokenFor(tn))
	if err != nil || claims.TenantID != tn {
		t.Errorf("profile token claims = %+v, %v", claims, err)
	}
	for _, s := range res.Steps {
		if s.Status != provisioner.StepSucceeded {
			t.Errorf("step %s = %s", s.Key, s.Status)
		}
	}
	if len(res.Steps) != len(provisioner.StepKeys()) {
		t.Errorf("steps recorded = %d, defined = %d", len(res.Steps), len(provisioner.StepKeys()))
	}
}

func TestReplayWithSameKeyDoesNothing(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	first, err := w.p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	before := w.hub.ExternalCalls()
	second, err := w.p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.RunID != first.RunID {
		t.Fatalf("replay result = %+v", second)
	}
	if after := w.hub.ExternalCalls(); after != before {
		t.Fatalf("replay made %d external calls", after-before)
	}
}

func TestTransientErrorsAreRetried(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	w.hub.Fail("erp", "EnsureCompany", fmt.Errorf("erpnext 503: %w", ports.ErrTransient), 2)
	res, err := w.p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var attempts int
	for _, s := range res.Steps {
		if s.Key == "erp.company" {
			attempts = s.Attempts
		}
	}
	if attempts != 3 {
		t.Fatalf("erp attempts = %d, want 3", attempts)
	}
}

func TestExhaustedRetriesRollBack(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	w.hub.Fail("erp", "EnsureCompany", fmt.Errorf("down: %w", ports.ErrTransient), -1)
	_, err := w.p.Provision(context.Background(), req)
	var pe *provisioner.Error
	if !errors.As(err, &pe) || pe.Step != "erp.company" || pe.Status != provisioner.RunRolledBack {
		t.Fatalf("err = %v", err)
	}
	if n := w.hub.CallCount("erp", "EnsureCompany", req.Tenant.ID); n != 4 {
		t.Fatalf("erp attempts = %d, want 4 (default)", n)
	}
	assertNothingLeft(t, w, req.Tenant.ID)
}

func assertNothingLeft(t *testing.T, w *world, tn uuid.UUID) {
	t.Helper()
	for sys, ok := range w.exists(tn) {
		if ok {
			t.Errorf("%s still exists after rollback", sys)
		}
	}
	if w.store.Seeded(tn) {
		t.Error("seed not retired after rollback")
	}
	if got := w.store.TenantStatus(tn); got != "DRAFT" {
		t.Errorf("tenant status after rollback = %s, want DRAFT", got)
	}
}

func TestPermanentFailureInStageTwoRollsBackEverything(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	w.hub.Fail("support", "EnsureInboxes", errors.New("libredesk: bad request"), -1)
	_, err := w.p.Provision(context.Background(), req)
	var pe *provisioner.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Step != "support.inboxes" || pe.Status != provisioner.RunRolledBack || pe.CleanupErr != nil {
		t.Fatalf("error = %+v", pe)
	}
	if n := w.hub.CallCount("support", "EnsureInboxes", req.Tenant.ID); n != 1 {
		t.Errorf("a permanent error must not be retried, got %d attempts", n)
	}
	assertNothingLeft(t, w, req.Tenant.ID)
	if run, _ := w.store.Run(req.Tenant.ID, "k1"); run.Status != provisioner.RunRolledBack {
		t.Errorf("run status = %s", run.Status)
	}
}

func TestFailureInStageThreeRollsBackStageTwoToo(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	w.hub.Fail("crm", "ApplyPipelines", errors.New("pipeline invalid"), -1)
	_, err := w.p.Provision(context.Background(), req)
	var pe *provisioner.Error
	if !errors.As(err, &pe) || pe.Step != "crm.pipelines" {
		t.Fatalf("err = %v", err)
	}
	assertNothingLeft(t, w, req.Tenant.ID)

	// Compensation runs in reverse: the Hermes profile (stage 2) is removed before the identity org (stage 1).
	ops := w.hub.Ops()
	idx := func(op string) int {
		for i, o := range ops {
			if o == op {
				return i
			}
		}
		return -1
	}
	if idx("runtime.RemoveProfile") < 0 || idx("identity.RemoveOrganization") < 0 ||
		idx("runtime.RemoveProfile") > idx("identity.RemoveOrganization") {
		t.Fatalf("compensation order wrong: %v", ops)
	}
}

func TestCompensationFailureIsReportedAndTenantNotMarkedClean(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	// Fail in stage 3 so the stage-2 ERP step has certainly completed (a sibling failing in the same stage may
	// cancel it before it starts, leaving nothing to undo).
	w.hub.Fail("support", "SeedKnowledgeBase", errors.New("boom"), -1)
	w.hub.Fail("erp", "RemoveCompany", errors.New("erpnext locked"), -1)
	_, err := w.p.Provision(context.Background(), req)
	var pe *provisioner.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Status != provisioner.RunCompensationFailed || pe.CleanupErr == nil || !strings.Contains(pe.CleanupErr.Error(), "erp.company") {
		t.Fatalf("error = %+v", pe)
	}
	if got := w.store.TenantStatus(req.Tenant.ID); got != "PROVISIONING" {
		t.Errorf("a tenant with leftovers must not be reset to DRAFT; status = %s", got)
	}
	// Everything else was still cleaned up.
	if w.crm.HasWorkspace(req.Tenant.ID) || w.id.HasOrganization(req.Tenant.ID) {
		t.Error("other systems were not compensated after one cleanup failed")
	}
}

func TestFailedRunReplayReturnsStoredErrorAndNewKeyRetries(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	w.hub.Fail("crm", "EnsureWorkspace", errors.New("twenty: 400"), 1)
	if _, err := w.p.Provision(context.Background(), req); err == nil {
		t.Fatal("want failure")
	}
	calls := w.hub.ExternalCalls()

	// Same key: the stored failure is returned, nothing is re-run.
	_, err := w.p.Provision(context.Background(), req)
	var pe *provisioner.Error
	if !errors.As(err, &pe) || pe.Status != provisioner.RunRolledBack {
		t.Fatalf("replayed failure = %v", err)
	}
	if w.hub.ExternalCalls() != calls {
		t.Fatal("replay of a failed run touched external systems")
	}

	// A new key is a new attempt (the fault was one-shot) and succeeds.
	req.IdempotencyKey = "k2"
	if _, err := w.p.Provision(context.Background(), req); err != nil {
		t.Fatalf("retry with a new key: %v", err)
	}
	if got := w.store.TenantStatus(req.Tenant.ID); got != "ACTIVE" {
		t.Errorf("status = %s", got)
	}
}

func TestCrashedRunIsResumedWithoutRepeatingFinishedSteps(t *testing.T) {
	w := newWorld(t)
	req := w.request("k1")
	ctx := context.Background()

	// Simulate a worker that died after finishing the identity step.
	run, claimed, err := w.store.BeginRun(ctx, req.Tenant.ID, req.BlueprintID, "k1", time.Minute)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	_ = w.store.SetRunStatus(ctx, req.Tenant.ID, run.ID, provisioner.RunRunning)
	ref, _ := w.id.EnsureOrganization(ctx, req.Tenant, req.FounderSub)
	_ = w.store.SaveStep(ctx, req.Tenant.ID, run.ID, provisioner.StepRecord{
		Key: "identity.organization", Ordinal: 0, Status: provisioner.StepSucceeded, Ref: ref, Attempts: 1})
	idCalls := w.hub.CallCount("identity", "EnsureOrganization", req.Tenant.ID)

	// While the dead worker's lease is still fresh, nobody may take over.
	if _, err := w.p.Provision(ctx, req); !errors.Is(err, provisioner.ErrInProgress) {
		t.Fatalf("want ErrInProgress, got %v", err)
	}
	w.store.Advance(10 * time.Minute) // lease expires

	res, err := w.p.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RunID != run.ID {
		t.Fatal("resume must continue the same run")
	}
	if got := w.hub.CallCount("identity", "EnsureOrganization", req.Tenant.ID); got != idCalls {
		t.Fatalf("identity step was repeated (%d -> %d calls)", idCalls, got)
	}
	if got := w.store.TenantStatus(req.Tenant.ID); got != "ACTIVE" {
		t.Errorf("status = %s", got)
	}
}

type blockingERP struct{ ports.ERP }

func (blockingERP) EnsureCompany(ctx context.Context, _ ports.TenantInfo, _ domain.BusinessProfile, _ domain.Accounting) (ports.Ref, error) {
	<-ctx.Done() // a hung ERPNext
	return "", ctx.Err()
}

func TestTimeoutAbortsAndRollsBack(t *testing.T) {
	w := newWorld(t)
	w.p.ERP = blockingERP{w.erp}
	w.p.Timeout = 80 * time.Millisecond
	req := w.request("k1")
	start := time.Now()
	_, err := w.p.Provision(context.Background(), req)
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not enforced")
	}
	var pe *provisioner.Error
	if !errors.As(err, &pe) || !errors.Is(err, context.DeadlineExceeded) || pe.Status != provisioner.RunRolledBack {
		t.Fatalf("err = %v", err)
	}
	assertNothingLeft(t, w, req.Tenant.ID)
}

func TestIndependentSystemsProvisionInParallel(t *testing.T) {
	w := newWorld(t)
	w.hub.Latency = 100 * time.Millisecond
	req := w.request("k1")
	start := time.Now()
	if _, err := w.p.Provision(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// 10 external calls x 100ms would be >= 1s if run one after another; three stages should take ~300ms.
	if elapsed > 700*time.Millisecond {
		t.Fatalf("provisioning took %v: stages are not running in parallel", elapsed)
	}
}

func TestRejectsBadRequestsBeforeTouchingAnything(t *testing.T) {
	w := newWorld(t)
	good := w.request("k1")
	cases := map[string]func(r *provisioner.Request){
		"slug mismatch": func(r *provisioner.Request) { r.Tenant.Slug = "other" },
		"no key":        func(r *provisioner.Request) { r.IdempotencyKey = "" },
		"no founder":    func(r *provisioner.Request) { r.FounderSub = "" },
		"no tenant":     func(r *provisioner.Request) { r.Tenant.ID = uuid.Nil },
		"no blueprint":  func(r *provisioner.Request) { r.BlueprintID = uuid.Nil },
		"invalid blueprint": func(r *provisioner.Request) {
			r.Blueprint.Pricing = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := good
			mutate(&req)
			_, err := w.p.Provision(context.Background(), req)
			if !errors.Is(err, provisioner.ErrInvalidRequest) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if n := w.hub.ExternalCalls(); n != 0 {
		t.Fatalf("invalid requests made %d external calls", n)
	}
}

func TestTenantsProvisionedConcurrentlyStayIsolated(t *testing.T) {
	w := newWorld(t)
	a, b := w.request("ka"), w.request("kb")
	errs := make(chan error, 2)
	for _, r := range []provisioner.Request{a, b} {
		go func(r provisioner.Request) {
			_, err := w.p.Provision(context.Background(), r)
			errs <- err
		}(r)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if w.rt.TokenFor(a.Tenant.ID) == w.rt.TokenFor(b.Tenant.ID) {
		t.Fatal("tenants share an MCP token")
	}
	ca, _ := w.signer.Verify(w.rt.TokenFor(a.Tenant.ID))
	cb, _ := w.signer.Verify(w.rt.TokenFor(b.Tenant.ID))
	if ca.TenantID != a.Tenant.ID || cb.TenantID != b.Tenant.ID {
		t.Fatal("token bound to the wrong tenant")
	}
	ra, _ := w.erp.Company(a.Tenant.ID)
	rb, _ := w.erp.Company(b.Tenant.ID)
	if ra.Ref == rb.Ref {
		t.Fatal("tenants share an ERP company")
	}
}
