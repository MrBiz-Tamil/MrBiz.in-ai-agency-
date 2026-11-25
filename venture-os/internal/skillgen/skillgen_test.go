package skillgen_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/skillgen"
)

func TestRenderAllProducesOneValidSkillPerAgent(t *testing.T) {
	bp := fixtures.EXIMBlueprint()
	skills, err := skillgen.RenderAll(bp)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != len(bp.Agents) {
		t.Fatalf("got %d skills for %d agents", len(skills), len(bp.Agents))
	}
	for i := 1; i < len(skills); i++ {
		if skills[i-1].AgentKey >= skills[i].AgentKey {
			t.Fatal("skills must be ordered by agent key")
		}
	}
	for _, s := range skills {
		if err := skillgen.Lint(s.Markdown); err != nil {
			t.Errorf("%s: %v", s.AgentKey, err)
		}
		if s.SHA256 != sha256.Sum256([]byte(s.Markdown)) {
			t.Errorf("%s: hash does not match content", s.AgentKey)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	a, _ := skillgen.RenderAll(fixtures.EXIMBlueprint())
	b, _ := skillgen.RenderAll(fixtures.EXIMBlueprint())
	for i := range a {
		if a[i].SHA256 != b[i].SHA256 {
			t.Fatalf("skill %s differs between renders", a[i].AgentKey)
		}
	}
}

func TestSkillContent(t *testing.T) {
	bp := fixtures.EXIMBlueprint()
	sales, err := skillgen.Render(bp, "sales")
	if err != nil {
		t.Fatal(err)
	}
	md := sales.Markdown
	for _, want := range []string{
		"name: sales\n",
		"# Head of Sales — Kerala Spice Exports",
		"You report to `ceo`.",
		"Your direct reports: `support` (Customer Support Lead).",
		"Pricing rule `eu-landed-cost` (EXIM_LANDED_COST): margin floor 12.00%",
		"`erp_pricing_and_ledger`",
		"Never calculate money yourself",
		"`request_human_approval`",
		"Qualify inquiries",
		"Inquiry to quote",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("sales skill missing %q", want)
		}
	}
	// Least privilege: sales was not granted market research, so the skill must not advertise it.
	if strings.Contains(md, "`market_research` —") {
		t.Error("skill documents a tool the agent was not granted")
	}
	ceo, _ := skillgen.Render(bp, "ceo")
	if !strings.Contains(ceo.Markdown, "root of the org chart and report to the founder") {
		t.Error("root agent should report to the founder")
	}
}

func TestChangedBlueprintChangesSkillHash(t *testing.T) {
	bp := fixtures.EXIMBlueprint()
	before, _ := skillgen.Render(bp, "finance")
	bp.Pricing[0].MarginFloorBps = 2000
	after, _ := skillgen.Render(bp, "finance")
	if before.SHA256 == after.SHA256 {
		t.Fatal("a pricing change must change the skill (and so require re-approval)")
	}
}

func TestRefusesInvalidBlueprintAndUnknownAgent(t *testing.T) {
	bad := fixtures.EXIMBlueprint()
	bad.Agents[0].Tools = []string{domain.ToolCRMOps} // loses governance
	if _, err := skillgen.RenderAll(bad); err == nil || !domain.IsValidationError(unwrap(err)) {
		t.Fatalf("want validation error, got %v", err)
	}
	if _, err := skillgen.Render(fixtures.EXIMBlueprint(), "nobody"); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func unwrap(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}

func TestLintRejectsMalformedSkills(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":     "# hi\n",
		"unterminated":       "---\nname: a-b\n",
		"bad name":           "---\nname: Bad Name\ndescription: \"x\"\n---\n request_human_approval",
		"missing governance": "---\nname: ok-name\ndescription: \"x\"\n---\n# body",
		"unquoted desc":      "---\nname: ok-name\ndescription: x\n---\n request_human_approval",
	}
	for name, md := range cases {
		if err := skillgen.Lint(md); err == nil {
			t.Errorf("%s: lint accepted a malformed skill", name)
		}
	}
	good := "---\nname: ok-name\ndescription: \"x\"\n---\nuse request_human_approval"
	if err := skillgen.Lint(good); err != nil {
		t.Errorf("good skill rejected: %v", err)
	}
}
