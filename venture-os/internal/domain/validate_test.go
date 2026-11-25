package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fixtures"
)

func TestFixtureBlueprintIsValid(t *testing.T) {
	if err := fixtures.EXIMBlueprint().Validate(); err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
}

func TestValidateRejectsEachRule(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(b *domain.Blueprint)
		want   string
	}{
		{"bad slug", func(b *domain.Blueprint) { b.Business.Slug = "Bad Slug" }, "business.slug"},
		{"bad currency", func(b *domain.Blueprint) { b.Business.BaseCurrency = "rupee" }, "base_currency"},
		{"unsupported schema", func(b *domain.Blueprint) { b.SchemaVersion = "99" }, "schema_version"},
		{"duplicate account code", func(b *domain.Blueprint) {
			b.Accounting.ChartOfAccounts = append(b.Accounting.ChartOfAccounts, b.Accounting.ChartOfAccounts[1])
		}, "duplicate account code"},
		{"account under wrong-type parent", func(b *domain.Blueprint) { b.Accounting.ChartOfAccounts[1].ParentCode = "2000" }, "cannot sit under"},
		{"unknown parent", func(b *domain.Blueprint) { b.Accounting.ChartOfAccounts[1].ParentCode = "9999" }, "unknown parent"},
		{"no revenue account", func(b *domain.Blueprint) {
			var keep []domain.Account
			for _, a := range b.Accounting.ChartOfAccounts {
				if a.Type != domain.AccountRevenue {
					keep = append(keep, a)
				}
			}
			b.Accounting.ChartOfAccounts = keep
		}, "at least one REVENUE"},
		{"tax rate over 100%", func(b *domain.Blueprint) { b.Accounting.Tax.Rates[0].RateBps = 10001 }, "between 0 and 10000"},
		{"single-stage pipeline", func(b *domain.Blueprint) { b.CRM.Pipelines[0].Stages = b.CRM.Pipelines[0].Stages[:1] }, "at least two stages"},
		{"duplicate stage", func(b *domain.Blueprint) {
			b.CRM.Pipelines[0].Stages[1].Key = b.CRM.Pipelines[0].Stages[0].Key
		}, "duplicate stage"},
		{"no pricing", func(b *domain.Blueprint) { b.Pricing = nil }, "at least one rule"},
		{"margin target below floor", func(b *domain.Blueprint) {
			v := 100
			b.Pricing[0].MarginTargetBps = &v
		}, "margin_target_bps"},
		{"formula not an object", func(b *domain.Blueprint) { b.Pricing[0].Formula = json.RawMessage(`[1,2]`) }, "formula must be a JSON object"},
		{"no root agent", func(b *domain.Blueprint) { b.Agents[0].ReportsTo = "sales" }, "exactly one root"},
		{"two roots", func(b *domain.Blueprint) { b.Agents[1].ReportsTo = "" }, "exactly one root"},
		{"reporting cycle", func(b *domain.Blueprint) {
			b.Agents[0].ReportsTo = "support" // ceo -> support -> sales -> ceo
		}, "cycle"},
		{"unknown manager", func(b *domain.Blueprint) { b.Agents[2].ReportsTo = "nobody" }, "unknown agent"},
		{"unknown tool", func(b *domain.Blueprint) { b.Agents[1].Tools = append(b.Agents[1].Tools, "raw_sql") }, "unknown tool"},
		{"agent without governance tool", func(b *domain.Blueprint) {
			b.Agents[3].Tools = []string{domain.ToolERPPricing}
		}, "must be granted"},
		{"agent without responsibilities", func(b *domain.Blueprint) { b.Agents[3].Responsibilities = nil }, "responsibility"},
		{"workflow without steps", func(b *domain.Blueprint) { b.Workflows[0].Steps = nil }, "no steps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtures.EXIMBlueprint()
			tc.mutate(&b)
			err := b.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !domain.IsValidationError(err) {
				t.Fatalf("want *ValidationError, got %T", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportsAllProblemsAtOnce(t *testing.T) {
	b := fixtures.EXIMBlueprint()
	b.Business.Slug = "X"
	b.Pricing = nil
	b.Workflows[0].Steps = nil
	var ve *domain.ValidationError
	err := b.Validate()
	if !domain.IsValidationError(err) {
		t.Fatal("want validation error")
	}
	ve = err.(*domain.ValidationError)
	if len(ve.Problems) < 3 {
		t.Fatalf("want >=3 problems reported together, got %v", ve.Problems)
	}
}

func TestHashIsStableAndContentSensitive(t *testing.T) {
	a, b := fixtures.EXIMBlueprint(), fixtures.EXIMBlueprint()
	ha, err := a.Hash()
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := b.Hash()
	if ha != hb {
		t.Fatal("identical blueprints must hash identically")
	}
	b.Business.Name = "Different"
	hc, _ := b.Hash()
	if ha == hc {
		t.Fatal("changed blueprint must hash differently")
	}
}

func TestBlueprintJSONRoundTrip(t *testing.T) {
	orig := fixtures.EXIMBlueprint()
	raw, err := orig.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back domain.Blueprint
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped blueprint invalid: %v", err)
	}
	h1, _ := orig.Hash()
	h2, _ := back.Hash()
	if h1 != h2 {
		t.Fatal("hash changed across a JSON round trip")
	}
}
