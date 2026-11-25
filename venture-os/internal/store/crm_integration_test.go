package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/crm/builtin"
	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/skillgen"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

// The whole path a new business takes: approved blueprint -> provisioning saga (with the built-in CRM in the CRM
// slots) -> the tenant's sales team using the CRM on the pipelines its own blueprint defined.
func TestProvisionedTenantCanUseTheBuiltInCRM(t *testing.T) {
	s, pool := newStore(t)
	provider := builtin.New(pool, tenancy.Options{AssumeRole: testdb.Role})
	hub := fakes.NewHub()
	signer, _ := mcptoken.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	p := &provisioner.Provisioner{
		Identity: fakes.NewIdentity(hub), ERP: fakes.NewERP(hub), CRM: builtin.NewWorkspace(provider), Support: fakes.NewSupport(hub),
		Runtime: fakes.NewRuntime(hub), Store: s, Tokens: signer,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}

	tn, sb := approvedTenant(t, s, "founder")
	info, err := s.Tenant(ctx, tn)
	must(t, err)
	res, err := p.Provision(ctx, provisioner.Request{Tenant: info.Info(), FounderSub: "founder", BlueprintID: sb.ID, Blueprint: sb.Blueprint, IdempotencyKey: "prov-crm"})
	must(t, err)
	if res.Status != provisioner.RunSucceeded {
		t.Fatalf("provisioning with the built-in CRM: %+v", res)
	}

	c, err := provider.For(ctx, tn)
	must(t, err)
	pipes, err := c.Pipelines(ctx)
	must(t, err)
	if len(pipes) != 2 || pipes[0].Key != "export_deals" || pipes[1].Key != "supplier_onboarding" {
		t.Fatalf("the CRM serves %+v, want the blueprint's two pipelines in order", pipes)
	}

	co, _, err := c.CreateCompany(ctx, "co-1", crm.CompanyInput{Name: "Nordic Spice Importers AB", Country: "SE", Domain: "nordicspice.se"})
	must(t, err)
	deal, _, err := c.CreateDeal(ctx, "deal-1", crm.DealInput{
		Name: "Black pepper, 2 x 20ft", CompanyID: co.ID, Pipeline: "export_deals", Stage: "inquiry",
		Amount: &crm.Money{Minor: 4_800_000, Currency: "USD"}, Fields: map[string]string{"incoterm": "CIF", "destination_port": "Gothenburg"},
	})
	must(t, err)
	if _, _, err := c.CreateDeal(ctx, "deal-2", crm.DealInput{Name: "No incoterm", CompanyID: co.ID, Pipeline: "export_deals", Stage: "inquiry"}); !errors.Is(err, crm.ErrInvalid) {
		t.Fatalf("the blueprint's required custom field must be enforced: %v", err)
	}
	for _, stage := range []string{"quoted", "sample_sent", "po_received"} {
		if deal, err = c.MoveDeal(ctx, deal.ID, stage, "rep-1", deal.Version); err != nil {
			t.Fatalf("move to %s: %v", stage, err)
		}
	}
	if deal.Stage != "po_received" {
		t.Fatalf("stage = %s", deal.Stage)
	}
	if _, err := c.MoveDeal(ctx, deal.ID, "delivered", "rep-1", 0); !errors.Is(err, crm.ErrInvalid) {
		t.Fatalf("a stage the blueprint does not define must be refused: %v", err)
	}
	if h, err := c.DealHistory(ctx, deal.ID); err != nil || len(h) != 4 {
		t.Fatalf("history = %+v, %v", h, err)
	}

	// A second business, provisioned the same way, sees none of it.
	tn2, _, err := s.CreateTenant(ctx, "founder-2", "create-founder-2")
	must(t, err)
	bp2 := fixtures.EXIMBlueprint()
	bp2.Business.Slug = "second-spice-co" // one business address per tenant
	sb2, err := s.SaveBlueprint(ctx, tn2, nil, bp2, "founder-2", "bp-founder-2")
	must(t, err)
	sb2, err = s.ApproveBlueprint(ctx, tn2, sb2.ID, "founder-2")
	must(t, err)
	info2, _ := s.Tenant(ctx, tn2)
	if res, err := p.Provision(ctx, provisioner.Request{Tenant: info2.Info(), FounderSub: "founder-2", BlueprintID: sb2.ID, Blueprint: sb2.Blueprint, IdempotencyKey: "prov-crm-2"}); err != nil || res.Status != provisioner.RunSucceeded {
		t.Fatalf("second tenant: %+v %v", res, err)
	}
	c2, err := provider.For(ctx, tn2)
	must(t, err)
	if page, err := c2.ListDeals(ctx, crm.DealFilter{}, crm.PageRequest{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("the second tenant sees %d deals (%v)", len(page.Items), err)
	}
	if _, err := c2.GetDeal(ctx, deal.ID); !errors.Is(err, crm.ErrNotFound) {
		t.Fatalf("the second tenant read the first tenant's deal: %v", err)
	}
}

// The CRM steps of the saga are verification, not creation: if what the CRM would serve differs from the approved
// blueprint, provisioning must fail rather than put a sales team on the wrong pipeline.
func TestBuiltInCRMStepsVerifyPipelinesAgainstTheBlueprint(t *testing.T) {
	s, pool := newStore(t)
	provider := builtin.New(pool, tenancy.Options{AssumeRole: testdb.Role})
	ws := builtin.NewWorkspace(provider)
	tn, sb := approvedTenant(t, s, "founder")
	info, err := s.Tenant(ctx, tn)
	must(t, err)

	// Before the seed stage there are no pipelines, so the blueprint cannot match.
	ref, err := ws.EnsureWorkspace(ctx, info.Info())
	must(t, err)
	if err := ws.ApplyPipelines(ctx, info.Info(), ref, sb.Blueprint.CRM.Pipelines); err == nil {
		t.Fatal("an unseeded tenant must not pass pipeline verification")
	}

	skills, err := skillgen.RenderAll(sb.Blueprint)
	must(t, err)
	must(t, s.SeedTenant(ctx, tn, sb.ID, sb.Blueprint, skills))
	must(t, ws.ApplyPipelines(ctx, info.Info(), ref, sb.Blueprint.CRM.Pipelines))
	must(t, ws.ApplyPipelines(ctx, info.Info(), ref, sb.Blueprint.CRM.Pipelines)) // re-applying is harmless

	differs := map[string]func(ps []domain.Pipeline) []domain.Pipeline{
		"renamed stage": func(ps []domain.Pipeline) []domain.Pipeline { ps[0].Stages[1].Name = "Quote sent"; return ps },
		"reordered stage": func(ps []domain.Pipeline) []domain.Pipeline {
			ps[0].Stages[0], ps[0].Stages[1] = ps[0].Stages[1], ps[0].Stages[0]
			return ps
		},
		"changed odds": func(ps []domain.Pipeline) []domain.Pipeline { ps[0].Stages[0].WinProbability++; return ps },
		"extra field": func(ps []domain.Pipeline) []domain.Pipeline {
			ps[0].CustomFields = append(ps[0].CustomFields, domain.CustomField{Key: "x", Label: "X", Type: "TEXT"})
			return ps
		},
		"swapped pipelines": func(ps []domain.Pipeline) []domain.Pipeline { ps[0], ps[1] = ps[1], ps[0]; return ps },
		"missing pipeline":  func(ps []domain.Pipeline) []domain.Pipeline { return ps[:1] },
	}
	for name, f := range differs {
		if err := ws.ApplyPipelines(ctx, info.Info(), ref, f(clonePipelines(sb.Blueprint.CRM.Pipelines))); err == nil {
			t.Errorf("%s: a blueprint that differs from the CRM's pipelines must be refused", name)
		}
	}
	if err := ws.ApplyPipelines(ctx, info.Info(), "builtin:someone-else", sb.Blueprint.CRM.Pipelines); err == nil {
		t.Error("another tenant's workspace reference must be refused")
	}

	// Rollback must never delete a business's customers.
	co, _, err := mustCRM(t, provider, tn).CreateCompany(ctx, "keep", crm.CompanyInput{Name: "Keep me"})
	must(t, err)
	must(t, ws.RemoveWorkspace(ctx, info.Info(), ref))
	if _, err := mustCRM(t, provider, tn).GetCompany(ctx, co.ID); err != nil {
		t.Fatalf("RemoveWorkspace deleted data: %v", err)
	}
}

func mustCRM(t *testing.T, p *builtin.Provider, tenant uuid.UUID) crm.CRM {
	t.Helper()
	c, err := p.For(ctx, tenant)
	must(t, err)
	return c
}

func clonePipelines(in []domain.Pipeline) []domain.Pipeline {
	out := make([]domain.Pipeline, len(in))
	for i, p := range in {
		p.Stages = append([]domain.Stage(nil), p.Stages...)
		p.CustomFields = append([]domain.CustomField(nil), p.CustomFields...)
		out[i] = p
	}
	return out
}
