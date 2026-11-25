// Package domain holds the pure business types shared by every Venture OS service. It imports nothing from the
// rest of the module and does no I/O, so it is trivially testable and safe to use from agents' tool layers.
package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// BlueprintSchemaVersion is bumped on any breaking change to Blueprint's JSON shape.
const BlueprintSchemaVersion = "1"

// Blueprint is the complete, machine-checkable design of one business: the output of the Genesis interview and
// the single input of the Provisioner. Money is always minor units; rates are basis points (1% = 100).
type Blueprint struct {
	SchemaVersion string          `json:"schema_version"`
	Business      BusinessProfile `json:"business"`
	Accounting    Accounting      `json:"accounting"`
	CRM           CRM             `json:"crm"`
	Pricing       []PricingRule   `json:"pricing"`
	Support       Support         `json:"support"`
	Agents        []Agent         `json:"agents"`
	Workflows     []Workflow      `json:"workflows"`
}

type BusinessProfile struct {
	Name                 string     `json:"name"`
	Slug                 string     `json:"slug"`
	Industry             string     `json:"industry"`
	Country              string     `json:"country"` // ISO 3166-1 alpha-2
	BaseCurrency         string     `json:"base_currency"`
	Timezone             string     `json:"timezone"`
	Description          string     `json:"description"`
	TargetCustomers      []string   `json:"target_customers"`
	Offerings            []Offering `json:"offerings"`
	StartingCapitalMinor int64      `json:"starting_capital_minor"`
}

type Offering struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // PRODUCT | SERVICE | SUBSCRIPTION
}

type Accounting struct {
	ChartOfAccounts []Account      `json:"chart_of_accounts"`
	Tax             TaxConfig      `json:"tax"`
	SystemAccounts  SystemAccounts `json:"system_accounts"`
}

// SystemAccounts names, by code, the accounts the finance engine posts to automatically. Without this mapping
// an approved invoice could not be turned into a journal without guessing at the chart of accounts.
type SystemAccounts struct {
	Cash       string `json:"cash"`        // ASSET: bank/cash; its balance is the opening point of cash-flow forecasts
	Receivable string `json:"receivable"`  // ASSET
	Payable    string `json:"payable"`     // LIABILITY
	Revenue    string `json:"revenue"`     // REVENUE
	TaxPayable string `json:"tax_payable"` // LIABILITY
}

type AccountType string

const (
	AccountAsset     AccountType = "ASSET"
	AccountLiability AccountType = "LIABILITY"
	AccountEquity    AccountType = "EQUITY"
	AccountRevenue   AccountType = "REVENUE"
	AccountExpense   AccountType = "EXPENSE"
)

type Account struct {
	Code       string      `json:"code"`
	Name       string      `json:"name"`
	Type       AccountType `json:"type"`
	ParentCode string      `json:"parent_code,omitempty"`
	Postable   bool        `json:"postable"`
}

type TaxConfig struct {
	Regime    string    `json:"regime"` // e.g. "GST", "VAT", "SALES_TAX", "NONE"
	Inclusive bool      `json:"prices_include_tax"`
	Rates     []TaxRate `json:"rates"`
}

type TaxRate struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	RateBps int    `json:"rate_bps"`
}

type CRM struct {
	Pipelines []Pipeline `json:"pipelines"`
}

type EntityType string

const (
	EntityLead   EntityType = "LEAD"
	EntityDeal   EntityType = "DEAL"
	EntityOrder  EntityType = "ORDER"
	EntityTicket EntityType = "TICKET"
	EntityCustom EntityType = "CUSTOM"
)

type Pipeline struct {
	Key          string        `json:"key"`
	Name         string        `json:"name"`
	EntityType   EntityType    `json:"entity_type"`
	Stages       []Stage       `json:"stages"`
	CustomFields []CustomField `json:"custom_fields"`
}

type Stage struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	WinProbability int    `json:"win_probability_bps"`
}

type CustomField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // TEXT | NUMBER | DATE | SELECT | BOOLEAN
	Required bool   `json:"required"`
}

type PricingModel string

const (
	PricingSaaSTier   PricingModel = "SAAS_TIER"
	PricingD2CUnit    PricingModel = "D2C_UNIT"
	PricingEXIMLanded PricingModel = "EXIM_LANDED_COST"
	PricingCustom     PricingModel = "CUSTOM"
)

// PricingRule configures the deterministic finance engine. Formula is model-specific and validated by the
// finance engine (step 5); the blueprint only guarantees it is a JSON object.
type PricingRule struct {
	Name                   string          `json:"name"`
	Model                  PricingModel    `json:"model"`
	Currency               string          `json:"currency"`
	Formula                json.RawMessage `json:"formula"`
	MarginFloorBps         int             `json:"margin_floor_bps"`
	MarginTargetBps        *int            `json:"margin_target_bps,omitempty"`
	ApprovalThresholdMinor *int64          `json:"approval_threshold_minor,omitempty"`
}

type Support struct {
	Channels []Channel `json:"channels"`
	KBDocs   []KBDoc   `json:"kb_docs"`
}

type ChannelKind string

const (
	ChannelWhatsApp ChannelKind = "WHATSAPP"
	ChannelEmail    ChannelKind = "EMAIL"
	ChannelChat     ChannelKind = "CHAT"
)

type Channel struct {
	Kind        ChannelKind `json:"kind"`
	Name        string      `json:"name"`
	RouteToTeam string      `json:"route_to_team"`
}

type KBDoc struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Tool names are the MCP tool families exposed to agents (see internal/mcpbus).
const (
	ToolMarketResearch = "market_research"
	ToolCRMOps         = "crm_ops"
	ToolSupportOps     = "support_ops"
	ToolERPPricing     = "erp_pricing_and_ledger"
	ToolGovernance     = "governance"
	ToolMeetings       = "meetings_ops"
	ToolNotes          = "notes_ops"
)

// AllTools lists every tool family an agent may be granted.
var AllTools = []string{ToolMarketResearch, ToolCRMOps, ToolSupportOps, ToolERPPricing, ToolGovernance, ToolMeetings, ToolNotes}

type Agent struct {
	Key                string   `json:"key"`
	RoleTitle          string   `json:"role_title"`
	ReportsTo          string   `json:"reports_to,omitempty"` // empty only for the single root (CEO)
	Responsibilities   []string `json:"responsibilities"`
	Tools              []string `json:"tools"`
	MonthlyBudgetMinor int64    `json:"monthly_budget_minor"`
}

type Workflow struct {
	Key              string   `json:"key"`
	Name             string   `json:"name"`
	Trigger          string   `json:"trigger"`
	Steps            []string `json:"steps"`
	RequiresApproval bool     `json:"requires_approval"`
}

// CanonicalJSON is the byte-stable encoding used for hashing: struct field order is fixed, and the free-form
// pricing formulas are re-encoded with sorted keys and compact whitespace, so the bytes (and hash) are identical
// before and after a round trip through Postgres jsonb, which normalises both.
func (b Blueprint) CanonicalJSON() ([]byte, error) {
	c := b
	c.Pricing = make([]PricingRule, len(b.Pricing))
	for i, r := range b.Pricing {
		norm, err := normalizeJSON(r.Formula)
		if err != nil {
			return nil, fmt.Errorf("blueprint: pricing rule %q formula: %w", r.Name, err)
		}
		r.Formula = norm
		c.Pricing[i] = r
	}
	return json.Marshal(c)
}

func normalizeJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep large integers exact
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// Hash is the sha256 of CanonicalJSON: the identity of a blueprint's content.
func (b Blueprint) Hash() ([32]byte, error) {
	raw, err := b.CanonicalJSON()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
