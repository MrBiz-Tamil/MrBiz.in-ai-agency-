package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on what a blueprint may contain. They protect three things: the database (organizations.name is capped at 200
// characters), the agents' instruction files (every free-text field below is copied into skill text, so it must be one
// plain line and short enough that a skill stays within its size limit), and the founder, who has to be able to read what
// they approve. Lengths are in characters. skillgen.Check is the exact renderability test; these keep ordinary
// blueprints far from it.
const (
	MaxNameChars        = 200 // business name, account name: the database limit for organizations.name
	MaxLabelChars       = 100 // industry, role title, channel, team, pricing rule, workflow name/trigger/step, ...
	MaxSentenceChars    = 300 // one responsibility, one target customer, one offering
	MaxDescriptionChars = 2000
	MaxKBBodyChars      = 10000 // a knowledge-base article body may span lines; it is not copied into skills

	MaxTargetCustomers = 20
	MaxOfferings       = 30
	MaxResponsibility  = 20 // responsibilities per agent
	MaxAgents          = 30
	MaxWorkflows       = 15
	MaxWorkflowSteps   = 10
	MaxPricingRules    = 20
	MaxChannels        = 20
	MaxKBDocs          = 40
	MaxAccounts        = 200
	MaxTaxRates        = 30
	MaxPipelines       = 10
	MaxStages          = 30
	MaxCustomFields    = 30
)

var (
	timezoneRe = regexp.MustCompile(`^[A-Za-z0-9_+/-]{1,64}$`)
	slugRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	countryRe  = regexp.MustCompile(`^[A-Z]{2}$`)
	codeRe     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	keyRe      = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)
	agentKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,62}$`)
)

// ValidationError aggregates every problem found, so the Genesis interview can show the founder (or feed back
// to the model) the full list in one round trip instead of one failure at a time.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid blueprint (%d problems): %s", len(e.Problems), strings.Join(e.Problems, "; "))
}

// Validate checks every structural and business rule the Provisioner relies on. A blueprint that passes can be
// provisioned without further interpretation; one that fails never reaches an external system.
func (b Blueprint) Validate() error {
	v := &validator{}
	if b.SchemaVersion != BlueprintSchemaVersion {
		v.addf("schema_version %q is not supported (want %q)", b.SchemaVersion, BlueprintSchemaVersion)
	}
	v.business(b.Business)
	v.accounting(b.Accounting, b.Business.BaseCurrency)
	v.crm(b.CRM)
	v.pricing(b.Pricing)
	v.support(b.Support)
	v.agents(b.Agents)
	v.workflows(b.Workflows)
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

// IsValidationError reports whether err is (or wraps) a *ValidationError.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

type validator struct{ problems []string }

func (v *validator) addf(format string, a ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, a...))
}

// singleLine reports whether s has no line break, control character, or invisible format character (zero-width and bidi
// controls). Text that fails it could start a new section when copied into an agent's skill, or hide from a reader.
func singleLine(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}

// line checks one free-text value: one plain line, at most max characters.
func (v *validator) line(field, s string, max int) {
	if n := utf8.RuneCountInString(s); n > max {
		v.addf("%s is %d characters; the limit is %d", field, n, max)
	}
	if !singleLine(s) {
		v.addf("%s must be one line of plain text (no line breaks, tabs or control characters)", field)
	}
}

// count checks how many items a list holds.
func (v *validator) count(field string, n, max int) {
	if n > max {
		v.addf("%s has %d entries; the limit is %d", field, n, max)
	}
}

func (v *validator) business(p BusinessProfile) {
	if strings.TrimSpace(p.Name) == "" {
		v.addf("business.name is required")
	}
	v.line("business.name", p.Name, MaxNameChars)
	v.line("business.industry", p.Industry, MaxLabelChars)
	v.line("business.description", p.Description, MaxDescriptionChars)
	if p.Timezone != "" && !timezoneRe.MatchString(p.Timezone) {
		v.addf("business.timezone %q must be an IANA time zone name such as Asia/Kolkata or UTC", p.Timezone)
	}
	v.count("business.target_customers", len(p.TargetCustomers), MaxTargetCustomers)
	for i, c := range p.TargetCustomers {
		v.line(fmt.Sprintf("business.target_customers[%d]", i), c, MaxSentenceChars)
	}
	v.count("business.offerings", len(p.Offerings), MaxOfferings)
	for i, o := range p.Offerings {
		v.line(fmt.Sprintf("business.offerings[%d].name", i), o.Name, MaxSentenceChars)
		switch o.Kind {
		case "PRODUCT", "SERVICE", "SUBSCRIPTION":
		default:
			v.addf("business.offerings[%d] %q has kind %q; use PRODUCT, SERVICE or SUBSCRIPTION", i, o.Name, o.Kind)
		}
	}
	if !slugRe.MatchString(p.Slug) {
		v.addf("business.slug %q must be 3-40 chars of a-z, 0-9 and '-', not starting or ending with '-'", p.Slug)
	}
	if !countryRe.MatchString(p.Country) {
		v.addf("business.country %q must be an ISO 3166-1 alpha-2 code", p.Country)
	}
	if !currencyRe.MatchString(p.BaseCurrency) {
		v.addf("business.base_currency %q must be an ISO 4217 code", p.BaseCurrency)
	}
	if p.StartingCapitalMinor < 0 {
		v.addf("business.starting_capital_minor must not be negative")
	}
}

func (v *validator) accounting(a Accounting, baseCurrency string) {
	if len(a.ChartOfAccounts) == 0 {
		v.addf("accounting.chart_of_accounts must not be empty")
	}
	codes := map[string]Account{}
	seenType := map[AccountType]bool{}
	v.count("accounting.chart_of_accounts", len(a.ChartOfAccounts), MaxAccounts)
	v.count("accounting.tax.rates", len(a.Tax.Rates), MaxTaxRates)
	v.line("accounting.tax.regime", a.Tax.Regime, MaxLabelChars)
	for _, r := range a.Tax.Rates {
		v.line(fmt.Sprintf("tax rate %q name", r.Code), r.Name, MaxLabelChars)
	}
	for _, acc := range a.ChartOfAccounts {
		v.line(fmt.Sprintf("account %q name", acc.Code), acc.Name, MaxNameChars)
		if !codeRe.MatchString(acc.Code) {
			v.addf("account code %q is invalid", acc.Code)
		}
		if _, dup := codes[acc.Code]; dup {
			v.addf("duplicate account code %q", acc.Code)
		}
		codes[acc.Code] = acc
		switch acc.Type {
		case AccountAsset, AccountLiability, AccountEquity, AccountRevenue, AccountExpense:
			seenType[acc.Type] = true
		default:
			v.addf("account %q has unknown type %q", acc.Code, acc.Type)
		}
		if strings.TrimSpace(acc.Name) == "" {
			v.addf("account %q needs a name", acc.Code)
		}
	}
	for _, acc := range a.ChartOfAccounts {
		if acc.ParentCode == "" {
			continue
		}
		parent, ok := codes[acc.ParentCode]
		switch {
		case !ok:
			v.addf("account %q has unknown parent %q", acc.Code, acc.ParentCode)
		case parent.Code == acc.Code:
			v.addf("account %q is its own parent", acc.Code)
		case parent.Type != acc.Type:
			v.addf("account %q (%s) cannot sit under %q (%s)", acc.Code, acc.Type, parent.Code, parent.Type)
		case parent.Postable:
			v.addf("parent account %q must be a non-postable group", parent.Code)
		}
	}
	for _, t := range []AccountType{AccountAsset, AccountRevenue, AccountExpense} {
		if len(a.ChartOfAccounts) > 0 && !seenType[t] {
			v.addf("chart of accounts needs at least one %s account", t)
		}
	}
	rates := map[string]bool{}
	for _, r := range a.Tax.Rates {
		if !codeRe.MatchString(r.Code) {
			v.addf("tax rate code %q is invalid", r.Code)
		}
		if rates[r.Code] {
			v.addf("duplicate tax rate code %q", r.Code)
		}
		rates[r.Code] = true
		if r.RateBps < 0 || r.RateBps > 10000 {
			v.addf("tax rate %q must be between 0 and 10000 bps", r.Code)
		}
	}
	if strings.TrimSpace(a.Tax.Regime) == "" {
		v.addf("accounting.tax.regime is required (use \"NONE\" if untaxed)")
	}
	_ = baseCurrency
	for _, req := range []struct {
		field, code string
		want        AccountType
	}{
		{"cash", a.SystemAccounts.Cash, AccountAsset},
		{"receivable", a.SystemAccounts.Receivable, AccountAsset},
		{"payable", a.SystemAccounts.Payable, AccountLiability},
		{"revenue", a.SystemAccounts.Revenue, AccountRevenue},
		{"tax_payable", a.SystemAccounts.TaxPayable, AccountLiability},
	} {
		acc, ok := codes[req.code]
		switch {
		case req.code == "":
			v.addf("accounting.system_accounts.%s is required", req.field)
		case !ok:
			v.addf("accounting.system_accounts.%s refers to unknown account %q", req.field, req.code)
		case acc.Type != req.want:
			v.addf("accounting.system_accounts.%s (%s) must be a %s account, not %s", req.field, req.code, req.want, acc.Type)
		case !acc.Postable:
			v.addf("accounting.system_accounts.%s (%s) must be a postable account", req.field, req.code)
		}
	}
}

func (v *validator) crm(c CRM) {
	keys := map[string]bool{}
	v.count("crm.pipelines", len(c.Pipelines), MaxPipelines)
	for _, p := range c.Pipelines {
		v.line(fmt.Sprintf("pipeline %q name", p.Key), p.Name, MaxLabelChars)
		v.count(fmt.Sprintf("pipeline %q stages", p.Key), len(p.Stages), MaxStages)
		v.count(fmt.Sprintf("pipeline %q custom fields", p.Key), len(p.CustomFields), MaxCustomFields)
		for _, s := range p.Stages {
			v.line(fmt.Sprintf("pipeline %q stage %q name", p.Key, s.Key), s.Name, MaxLabelChars)
		}
		for _, f := range p.CustomFields {
			v.line(fmt.Sprintf("pipeline %q field %q label", p.Key, f.Key), f.Label, MaxLabelChars)
		}
		if !keyRe.MatchString(p.Key) {
			v.addf("pipeline key %q is invalid", p.Key)
		}
		if keys[p.Key] {
			v.addf("duplicate pipeline key %q", p.Key)
		}
		keys[p.Key] = true
		switch p.EntityType {
		case EntityLead, EntityDeal, EntityOrder, EntityTicket, EntityCustom:
		default:
			v.addf("pipeline %q has unknown entity_type %q", p.Key, p.EntityType)
		}
		if len(p.Stages) < 2 {
			v.addf("pipeline %q needs at least two stages", p.Key)
		}
		stageKeys := map[string]bool{}
		for _, s := range p.Stages {
			if !keyRe.MatchString(s.Key) {
				v.addf("pipeline %q stage key %q is invalid", p.Key, s.Key)
			}
			if stageKeys[s.Key] {
				v.addf("pipeline %q has duplicate stage %q", p.Key, s.Key)
			}
			stageKeys[s.Key] = true
			if s.WinProbability < 0 || s.WinProbability > 10000 {
				v.addf("pipeline %q stage %q win probability must be 0-10000 bps", p.Key, s.Key)
			}
		}
		fieldKeys := map[string]bool{}
		for _, f := range p.CustomFields {
			if !keyRe.MatchString(f.Key) {
				v.addf("pipeline %q custom field key %q is invalid", p.Key, f.Key)
			}
			if fieldKeys[f.Key] {
				v.addf("pipeline %q has duplicate custom field %q", p.Key, f.Key)
			}
			fieldKeys[f.Key] = true
			switch f.Type {
			case "TEXT", "NUMBER", "DATE", "SELECT", "BOOLEAN":
			default:
				v.addf("pipeline %q field %q has unknown type %q", p.Key, f.Key, f.Type)
			}
		}
	}
}

func (v *validator) pricing(rules []PricingRule) {
	if len(rules) == 0 {
		v.addf("pricing must define at least one rule (agents never invent prices)")
	}
	names := map[string]bool{}
	v.count("pricing", len(rules), MaxPricingRules)
	for _, r := range rules {
		if strings.TrimSpace(r.Name) == "" {
			v.addf("a pricing rule has no name")
		}
		v.line(fmt.Sprintf("pricing rule %q name", r.Name), r.Name, MaxLabelChars)
		if names[r.Name] {
			v.addf("duplicate pricing rule %q", r.Name)
		}
		names[r.Name] = true
		switch r.Model {
		case PricingSaaSTier, PricingD2CUnit, PricingEXIMLanded, PricingCustom:
		default:
			v.addf("pricing rule %q has unknown model %q", r.Name, r.Model)
		}
		if !currencyRe.MatchString(r.Currency) {
			v.addf("pricing rule %q has invalid currency %q", r.Name, r.Currency)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(r.Formula, &obj); err != nil || obj == nil {
			v.addf("pricing rule %q formula must be a JSON object", r.Name)
		}
		if r.MarginFloorBps < 0 || r.MarginFloorBps > 10000 {
			v.addf("pricing rule %q margin_floor_bps must be 0-10000", r.Name)
		}
		if r.MarginTargetBps != nil && (*r.MarginTargetBps < r.MarginFloorBps || *r.MarginTargetBps > 10000) {
			v.addf("pricing rule %q margin_target_bps must be between the floor and 10000", r.Name)
		}
		if r.ApprovalThresholdMinor != nil && *r.ApprovalThresholdMinor < 0 {
			v.addf("pricing rule %q approval threshold must not be negative", r.Name)
		}
	}
}

func (v *validator) support(s Support) {
	slugs := map[string]bool{}
	v.count("support.kb_docs", len(s.KBDocs), MaxKBDocs)
	v.count("support.channels", len(s.Channels), MaxChannels)
	for _, c := range s.Channels {
		v.line(fmt.Sprintf("channel %q name", c.Name), c.Name, MaxLabelChars)
		v.line(fmt.Sprintf("channel %q route_to_team", c.Name), c.RouteToTeam, MaxLabelChars)
	}
	for _, d := range s.KBDocs {
		v.line(fmt.Sprintf("kb doc %q title", d.Slug), d.Title, MaxNameChars)
		if n := utf8.RuneCountInString(d.Body); n > MaxKBBodyChars {
			v.addf("kb doc %q body is %d characters; the limit is %d", d.Slug, n, MaxKBBodyChars)
		}
		if !slugRe.MatchString(d.Slug) {
			v.addf("kb doc slug %q is invalid", d.Slug)
		}
		if slugs[d.Slug] {
			v.addf("duplicate kb doc slug %q", d.Slug)
		}
		slugs[d.Slug] = true
		if strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.Body) == "" {
			v.addf("kb doc %q needs a title and a body", d.Slug)
		}
	}
	names := map[string]bool{}
	for _, c := range s.Channels {
		switch c.Kind {
		case ChannelWhatsApp, ChannelEmail, ChannelChat:
		default:
			v.addf("channel %q has unknown kind %q", c.Name, c.Kind)
		}
		if strings.TrimSpace(c.Name) == "" {
			v.addf("a support channel has no name")
		}
		if names[c.Name] {
			v.addf("duplicate channel name %q", c.Name)
		}
		names[c.Name] = true
	}
}

func (v *validator) agents(agents []Agent) {
	if len(agents) == 0 {
		v.addf("agents must define an org chart with at least a root agent")
		return
	}
	known := map[string]Agent{}
	v.count("agents", len(agents), MaxAgents)
	for _, a := range agents {
		v.line(fmt.Sprintf("agent %q role_title", a.Key), a.RoleTitle, MaxLabelChars)
		v.count(fmt.Sprintf("agent %q responsibilities", a.Key), len(a.Responsibilities), MaxResponsibility)
		for i, r := range a.Responsibilities {
			v.line(fmt.Sprintf("agent %q responsibilities[%d]", a.Key, i), r, MaxSentenceChars)
		}
		if !agentKeyRe.MatchString(a.Key) {
			v.addf("agent key %q is invalid", a.Key)
		}
		if _, dup := known[a.Key]; dup {
			v.addf("duplicate agent key %q", a.Key)
		}
		known[a.Key] = a
	}
	roots := 0
	for _, a := range agents {
		if strings.TrimSpace(a.RoleTitle) == "" {
			v.addf("agent %q needs a role_title", a.Key)
		}
		if a.MonthlyBudgetMinor < 0 {
			v.addf("agent %q has a negative budget", a.Key)
		}
		if len(a.Responsibilities) == 0 {
			v.addf("agent %q needs at least one responsibility (it becomes its skill)", a.Key)
		}
		hasGov := false
		seen := map[string]bool{}
		for _, t := range a.Tools {
			if !isTool(t) {
				v.addf("agent %q requests unknown tool %q", a.Key, t)
			}
			if seen[t] {
				v.addf("agent %q lists tool %q twice", a.Key, t)
			}
			seen[t] = true
			hasGov = hasGov || t == ToolGovernance
		}
		if !hasGov {
			v.addf("agent %q must be granted %q so it can request approval and escalate", a.Key, ToolGovernance)
		}
		switch {
		case a.ReportsTo == "":
			roots++
		case a.ReportsTo == a.Key:
			v.addf("agent %q reports to itself", a.Key)
		default:
			if _, ok := known[a.ReportsTo]; !ok {
				v.addf("agent %q reports to unknown agent %q", a.Key, a.ReportsTo)
			}
		}
	}
	if roots != 1 {
		v.addf("org chart must have exactly one root (an agent with no reports_to), found %d", roots)
	}
	// Cycle check: walking up from any agent must terminate at a root within len(agents) hops.
	for _, a := range agents {
		cur, hops := a, 0
		for cur.ReportsTo != "" && hops <= len(agents) {
			next, ok := known[cur.ReportsTo]
			if !ok {
				break
			}
			cur = next
			hops++
		}
		if hops > len(agents) {
			v.addf("org chart has a reporting cycle through %q", a.Key)
			break
		}
	}
}

func (v *validator) workflows(ws []Workflow) {
	keys := map[string]bool{}
	v.count("workflows", len(ws), MaxWorkflows)
	for _, w := range ws {
		v.line(fmt.Sprintf("workflow %q name", w.Key), w.Name, MaxLabelChars)
		v.line(fmt.Sprintf("workflow %q trigger", w.Key), w.Trigger, MaxLabelChars)
		v.count(fmt.Sprintf("workflow %q steps", w.Key), len(w.Steps), MaxWorkflowSteps)
		for i, s := range w.Steps {
			v.line(fmt.Sprintf("workflow %q steps[%d]", w.Key, i), s, MaxLabelChars)
		}
		if !keyRe.MatchString(w.Key) {
			v.addf("workflow key %q is invalid", w.Key)
		}
		if keys[w.Key] {
			v.addf("duplicate workflow key %q", w.Key)
		}
		keys[w.Key] = true
		if len(w.Steps) == 0 {
			v.addf("workflow %q has no steps", w.Key)
		}
		if strings.TrimSpace(w.Trigger) == "" {
			v.addf("workflow %q has no trigger", w.Key)
		}
	}
}

func isTool(name string) bool {
	for _, t := range AllTools {
		if t == name {
			return true
		}
	}
	return false
}
