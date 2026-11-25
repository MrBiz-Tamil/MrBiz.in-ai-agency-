// Package fixtures builds realistic, valid domain objects for tests and local demos. It is a normal package (not
// _test) so every package's tests can share the same fixtures.
package fixtures

import (
	"encoding/json"

	"github.com/mrbiz/venture-os/internal/domain"
)

func ptr[T any](v T) *T { return &v }

// EXIMBlueprint is a small export-import trading company in India selling to the EU: it exercises GST taxes,
// a landed-cost pricing rule, a two-pipeline CRM, WhatsApp+email support and a five-agent org chart.
func EXIMBlueprint() domain.Blueprint {
	allTools := []string{domain.ToolMarketResearch, domain.ToolCRMOps, domain.ToolSupportOps, domain.ToolERPPricing, domain.ToolGovernance}
	return domain.Blueprint{
		SchemaVersion: domain.BlueprintSchemaVersion,
		Business: domain.BusinessProfile{
			Name:                 "Kerala Spice Exports",
			Slug:                 "kerala-spice-exports",
			Industry:             "export-import",
			Country:              "IN",
			BaseCurrency:         "INR",
			Timezone:             "Asia/Kolkata",
			Description:          "Sources whole spices from Kerala farms and exports to EU wholesalers.",
			TargetCustomers:      []string{"EU spice wholesalers", "specialty food importers"},
			Offerings:            []domain.Offering{{Name: "Black pepper (bulk)", Kind: "PRODUCT"}, {Name: "Cardamom (bulk)", Kind: "PRODUCT"}},
			StartingCapitalMinor: 500000000, // INR 5,000,000.00
		},
		Accounting: domain.Accounting{
			ChartOfAccounts: []domain.Account{
				{Code: "1000", Name: "Current assets", Type: domain.AccountAsset},
				{Code: "1010", Name: "Bank - INR", Type: domain.AccountAsset, ParentCode: "1000", Postable: true},
				{Code: "1200", Name: "Accounts receivable", Type: domain.AccountAsset, ParentCode: "1000", Postable: true},
				{Code: "1400", Name: "Inventory", Type: domain.AccountAsset, ParentCode: "1000", Postable: true},
				{Code: "2000", Name: "Current liabilities", Type: domain.AccountLiability},
				{Code: "2100", Name: "Accounts payable", Type: domain.AccountLiability, ParentCode: "2000", Postable: true},
				{Code: "2200", Name: "GST payable", Type: domain.AccountLiability, ParentCode: "2000", Postable: true},
				{Code: "3000", Name: "Owner's equity", Type: domain.AccountEquity, Postable: true},
				{Code: "4000", Name: "Export sales", Type: domain.AccountRevenue, Postable: true},
				{Code: "5000", Name: "Cost of goods sold", Type: domain.AccountExpense, Postable: true},
				{Code: "5100", Name: "Freight and insurance", Type: domain.AccountExpense, Postable: true},
			},
			SystemAccounts: domain.SystemAccounts{Cash: "1010", Receivable: "1200", Payable: "2100", Revenue: "4000", TaxPayable: "2200"},
			Tax: domain.TaxConfig{
				Regime: "GST",
				Rates: []domain.TaxRate{
					{Code: "GST0", Name: "Zero-rated export (LUT)", RateBps: 0},
					{Code: "GST5", Name: "GST 5%", RateBps: 500},
				},
			},
		},
		CRM: domain.CRM{Pipelines: []domain.Pipeline{
			{
				Key: "export_deals", Name: "Export deals", EntityType: domain.EntityDeal,
				Stages: []domain.Stage{
					{Key: "inquiry", Name: "Inquiry", WinProbability: 1000},
					{Key: "quoted", Name: "Quoted", WinProbability: 3000},
					{Key: "sample_sent", Name: "Sample sent", WinProbability: 5500},
					{Key: "po_received", Name: "PO received", WinProbability: 9000},
					{Key: "shipped", Name: "Shipped", WinProbability: 10000},
				},
				CustomFields: []domain.CustomField{
					{Key: "incoterm", Label: "Incoterm", Type: "SELECT", Required: true},
					{Key: "destination_port", Label: "Destination port", Type: "TEXT"},
				},
			},
			{
				Key: "supplier_onboarding", Name: "Supplier onboarding", EntityType: domain.EntityLead,
				Stages: []domain.Stage{
					{Key: "identified", Name: "Identified", WinProbability: 2000},
					{Key: "audited", Name: "Quality audited", WinProbability: 6000},
					{Key: "active", Name: "Active supplier", WinProbability: 10000},
				},
			},
		}},
		Pricing: []domain.PricingRule{{
			Name:     "eu-landed-cost",
			Model:    domain.PricingEXIMLanded,
			Currency: "USD",
			Formula: json.RawMessage(`{"incoterms":["FOB","CIF","DDP"],"fx_buffer_bps":150,"export_handling_minor":20000,` +
				`"insurance_bps":50,"duty_bps":500,` +
				`"components":["goods","packing","inland_freight","ocean_freight","insurance","duty","handling"]}`),
			MarginFloorBps:         1200,
			MarginTargetBps:        ptr(1800),
			ApprovalThresholdMinor: ptr(int64(1000000)), // USD 10,000.00
		}},
		Support: domain.Support{
			Channels: []domain.Channel{
				{Kind: domain.ChannelEmail, Name: "Sales inbox", RouteToTeam: "sales"},
				{Kind: domain.ChannelWhatsApp, Name: "Buyers WhatsApp", RouteToTeam: "sales"},
			},
			KBDocs: []domain.KBDoc{{
				Slug: "shipping-and-incoterms", Title: "Shipping terms and Incoterms",
				Body: "We quote FOB Kochi, CIF and DDP. Standard lead time is 21 days from PO. Payment: 30% advance, 70% against BL copy.",
			}},
		},
		Agents: []domain.Agent{
			{Key: "ceo", RoleTitle: "Chief Executive Agent", Responsibilities: []string{"Set weekly priorities", "Escalate blockers to the founder"}, Tools: allTools, MonthlyBudgetMinor: 20000},
			{Key: "sales", RoleTitle: "Head of Sales", ReportsTo: "ceo", Responsibilities: []string{"Qualify inquiries", "Draft quotes from the pricing engine"}, Tools: []string{domain.ToolCRMOps, domain.ToolSupportOps, domain.ToolERPPricing, domain.ToolGovernance}, MonthlyBudgetMinor: 15000},
			{Key: "sourcing", RoleTitle: "Head of Sourcing", ReportsTo: "ceo", Responsibilities: []string{"Find and vet suppliers", "Track harvest prices"}, Tools: []string{domain.ToolMarketResearch, domain.ToolCRMOps, domain.ToolGovernance}, MonthlyBudgetMinor: 10000},
			{Key: "finance", RoleTitle: "Finance Controller", ReportsTo: "ceo", Responsibilities: []string{"Prepare invoices and POs", "Forecast cash flow"}, Tools: []string{domain.ToolERPPricing, domain.ToolGovernance}, MonthlyBudgetMinor: 8000},
			{Key: "support", RoleTitle: "Customer Support Lead", ReportsTo: "sales", Responsibilities: []string{"Triage the shared inbox", "Answer from the knowledge base"}, Tools: []string{domain.ToolSupportOps, domain.ToolGovernance}, MonthlyBudgetMinor: 5000},
		},
		Workflows: []domain.Workflow{{
			Key: "inquiry_to_quote", Name: "Inquiry to quote", Trigger: "support.rfq_received",
			Steps:            []string{"parse_rfq", "compute_landed_cost", "draft_quote", "request_approval"},
			RequiresApproval: true,
		}},
	}
}
