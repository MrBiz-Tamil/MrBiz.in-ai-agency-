package fakes_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/ports"
)

func tenant(slug string) ports.TenantInfo {
	return ports.TenantInfo{ID: uuid.New(), Slug: slug, Name: slug}
}

func TestEnsureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	hub := fakes.NewHub()
	erp, crm := fakes.NewERP(hub), fakes.NewCRM(hub)
	bp := fixtures.EXIMBlueprint()
	tn := tenant("a")

	r1, err := erp.EnsureCompany(ctx, tn, bp.Business, bp.Accounting)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := erp.EnsureCompany(ctx, tn, bp.Business, bp.Accounting)
	if err != nil || r1 != r2 {
		t.Fatalf("second ensure: ref %q vs %q, err %v", r1, r2, err)
	}
	co, ok := erp.Company(tn.ID)
	if !ok || len(co.Accounts) != len(bp.Accounting.ChartOfAccounts) {
		t.Fatalf("company state wrong: %+v", co)
	}

	ws, _ := crm.EnsureWorkspace(ctx, tn)
	for i := 0; i < 3; i++ {
		if err := crm.ApplyPipelines(ctx, tn, ws, bp.CRM.Pipelines); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(crm.Pipelines(tn.ID)); got != len(bp.CRM.Pipelines) {
		t.Fatalf("re-applying pipelines duplicated them: %d", got)
	}
}

func TestRemoveIsIdempotentAndReal(t *testing.T) {
	ctx := context.Background()
	hub := fakes.NewHub()
	crm := fakes.NewCRM(hub)
	tn := tenant("a")
	ws, _ := crm.EnsureWorkspace(ctx, tn)
	for i := 0; i < 2; i++ {
		if err := crm.RemoveWorkspace(ctx, tn, ws); err != nil {
			t.Fatalf("remove #%d: %v", i, err)
		}
	}
	if crm.HasWorkspace(tn.ID) {
		t.Fatal("workspace still present after removal")
	}
	if err := crm.ApplyPipelines(ctx, tn, ws, nil); !errors.Is(err, fakes.ErrNotFound) {
		t.Fatalf("applying to a removed workspace: want ErrNotFound, got %v", err)
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	ctx := context.Background()
	hub := fakes.NewHub()
	rt := fakes.NewRuntime(hub)
	a, b := tenant("a"), tenant("b")

	pa, _ := rt.EnsureProfile(ctx, a, "token-a")
	pb, _ := rt.EnsureProfile(ctx, b, "token-b")
	if pa == pb {
		t.Fatal("tenants share a profile ref")
	}
	if rt.TokenFor(a.ID) == rt.TokenFor(b.ID) {
		t.Fatal("tenants share an MCP token")
	}
	if err := rt.InstallSkills(ctx, a, pa, []ports.Skill{{AgentKey: "ceo", Markdown: "# a"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(rt.Skills(b.ID)); n != 0 {
		t.Fatalf("tenant b sees %d of tenant a's skills", n)
	}
	// Using another tenant's profile ref is rejected, not silently accepted.
	if err := rt.InstallSkills(ctx, b, pa, []ports.Skill{{AgentKey: "ceo"}}); !errors.Is(err, fakes.ErrNotFound) {
		t.Fatalf("cross-tenant profile ref: want ErrNotFound, got %v", err)
	}
}

func TestNilTenantIsRejected(t *testing.T) {
	hub := fakes.NewHub()
	if _, err := fakes.NewIdentity(hub).EnsureOrganization(context.Background(), ports.TenantInfo{}, "u"); err == nil {
		t.Fatal("nil tenant id accepted")
	}
}

func TestFaultInjection(t *testing.T) {
	ctx := context.Background()
	hub := fakes.NewHub()
	erp := fakes.NewERP(hub)
	bp := fixtures.EXIMBlueprint()
	tn := tenant("a")
	boom := fmt.Errorf("erpnext 503: %w", ports.ErrTransient)

	hub.Fail("erp", "EnsureCompany", boom, 2) // fail twice, then recover
	for i := 0; i < 2; i++ {
		if _, err := erp.EnsureCompany(ctx, tn, bp.Business, bp.Accounting); !errors.Is(err, ports.ErrTransient) {
			t.Fatalf("attempt %d: want transient error, got %v", i, err)
		}
	}
	if _, ok := erp.Company(tn.ID); ok {
		t.Fatal("a failed call must not leave state behind")
	}
	if _, err := erp.EnsureCompany(ctx, tn, bp.Business, bp.Accounting); err != nil {
		t.Fatalf("third attempt should succeed: %v", err)
	}
	if n := hub.CallCount("erp", "EnsureCompany", tn.ID); n != 3 {
		t.Fatalf("call count = %d, want 3", n)
	}

	hub.Fail("erp", "RemoveCompany", errors.New("permanent"), -1)
	if err := erp.RemoveCompany(ctx, tn, ""); err == nil {
		t.Fatal("permanent fault not returned")
	}
	hub.ClearFaults()
	if err := erp.RemoveCompany(ctx, tn, ""); err != nil {
		t.Fatalf("after ClearFaults: %v", err)
	}
}

func TestLatencyHonoursContextCancellation(t *testing.T) {
	hub := fakes.NewHub()
	hub.Latency = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := fakes.NewCRM(hub).EnsureWorkspace(ctx, tenant("a"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("call did not stop at the context deadline")
	}
}

func TestCallLogRecordsOrder(t *testing.T) {
	ctx := context.Background()
	hub := fakes.NewHub()
	crm, sup := fakes.NewCRM(hub), fakes.NewSupport(hub)
	tn := tenant("a")
	crm.EnsureWorkspace(ctx, tn)
	sup.EnsureInboxes(ctx, tn, nil)
	crm.RemoveWorkspace(ctx, tn, "")
	want := []string{"crm.EnsureWorkspace", "support.EnsureInboxes", "crm.RemoveWorkspace"}
	got := hub.Ops()
	if len(got) != len(want) {
		t.Fatalf("ops = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ops = %v, want %v", got, want)
		}
	}
}

func TestLLMReplaysScriptAndFailsWhenExhausted(t *testing.T) {
	ctx := context.Background()
	llm := fakes.NewLLM(fakes.NewHub())
	llm.Script(`{"step":1}`, `{"step":2}`)
	for i, want := range []string{`{"step":1}`, `{"step":2}`} {
		got, err := llm.Complete(ctx, ports.LLMRequest{Model: "m", Messages: []ports.Message{{Role: "user", Content: "hi"}}})
		if err != nil || got.Content != want {
			t.Fatalf("reply %d = %q, %v", i, got.Content, err)
		}
	}
	if _, err := llm.Complete(ctx, ports.LLMRequest{}); err == nil {
		t.Fatal("exhausted script must error, not invent an answer")
	}
	if n := len(llm.Requests()); n != 3 {
		t.Fatalf("recorded %d requests, want 3", n)
	}
}

func TestConcurrentEnsureCreatesOneResource(t *testing.T) {
	ctx := context.Background()
	hub := fakes.NewHub()
	crm := fakes.NewCRM(hub)
	tn := tenant("a")
	var wg sync.WaitGroup
	refs := make(chan ports.Ref, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := crm.EnsureWorkspace(ctx, tn)
			if err != nil {
				t.Error(err)
				return
			}
			refs <- r
		}()
	}
	wg.Wait()
	close(refs)
	var first ports.Ref
	for r := range refs {
		if first == "" {
			first = r
		}
		if r != first {
			t.Fatalf("concurrent ensure returned different refs: %q vs %q", r, first)
		}
	}
}
