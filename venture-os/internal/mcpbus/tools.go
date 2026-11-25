package mcpbus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/financeengine"
)

// argError is a problem with the arguments the agent supplied; its text is safe and useful to show the agent.
type argError struct{ msg string }

func (e *argError) Error() string { return e.msg }

type tool struct {
	Name        string
	Family      string // one of the domain.Tool* families
	Description string
	Schema      string // JSON Schema for the arguments
	ReadOnly    bool
	run         func(ctx context.Context, c *call, raw json.RawMessage) (any, error)
}

// Call runs the tool and returns its JSON text result.
func (t tool) Call(ctx context.Context, c *call, raw json.RawMessage) (string, error) {
	v, err := t.run(ctx, c, raw)
	if err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// typed adapts a function over a struct of arguments. Decoding is strict: an unknown key (for instance a
// "tenant_id" smuggled in by a prompt-injected agent) is an error, never silently ignored.
func typed[A any](fn func(ctx context.Context, c *call, a A) (any, error)) func(context.Context, *call, json.RawMessage) (any, error) {
	return func(ctx context.Context, c *call, raw json.RawMessage) (any, error) {
		var a A
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return nil, &argError{msg: "Invalid arguments: " + err.Error()}
		}
		if dec.More() {
			return nil, &argError{msg: "Invalid arguments: unexpected trailing data"}
		}
		return fn(ctx, c, a)
	}
}

// userMessage turns an error into text safe to show an agent. Validation and business-rule failures are shown
// verbatim (they say what to fix); anything unexpected is generic, so internals never leak into a prompt.
func userMessage(err error) string {
	var ae *argError
	if errors.As(err, &ae) {
		return ae.msg
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeAlreadyExists,
			connect.CodeFailedPrecondition, connect.CodePermissionDenied, connect.CodeResourceExhausted:
			return ce.Message()
		case connect.CodeUnauthenticated:
			return "Your credentials are invalid or expired. Escalate to the founder."
		}
	}
	return "The service had a problem and nothing was changed. Retry once; if it persists, call escalate_blocker."
}

func authed[T any](c *call, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+c.token)
	return req
}

// ───────────────────────────── schema helpers ─────────────────────────────

func schemaOf(props map[string]any, required ...string) string {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string, min int64) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func enum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "enum": vals, "description": desc}
}
func object(desc string, props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "description": desc, "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

const idemDesc = "A key unique to this intent. Reuse the SAME key when retrying the same request so it is not duplicated; use a new key for a different request."

// pricingProps are the arguments shared by calculate_price and create_quote: the rule plus exactly one model section.
func pricingProps() map[string]any {
	return map[string]any{
		"rule_name": str("Name of an active pricing rule (see list_pricing_rules)."),
		"saas": object("For SAAS_TIER rules.", map[string]any{
			"tier": str("Tier name"), "seats": integer("Number of seats", 1), "billing": enum("Billing term", "monthly", "annual"),
		}, "tier", "seats", "billing"),
		"d2c": object("For D2C_UNIT rules.", map[string]any{
			"quantity": integer("Units in the order", 1),
		}, "quantity"),
		"exim": object("For EXIM_LANDED_COST rules. All amounts are totals for the shipment, in minor units of the rule currency.", map[string]any{
			"incoterm":                  enum("Incoterm", "FOB", "CIF", "DDP"),
			"quantity":                  integer("Quantity (units of the goods)", 1),
			"unit_cost_minor":           integer("Cost per unit of goods, minor units", 0),
			"packing_minor":             integer("Total packing cost", 0),
			"inland_freight_minor":      integer("Total inland freight to the port", 0),
			"ocean_freight_minor":       integer("Total ocean freight. Required for CIF and DDP.", 0),
			"destination_charges_minor": integer("Total destination charges. Required for DDP.", 0),
		}, "incoterm", "quantity", "unit_cost_minor", "packing_minor", "inland_freight_minor"),
		"target_margin_bps": integer("Override the rule's target margin, in basis points (1% = 100). Cost-plus models only.", 0),
		"price_minor":       integer("Evaluate this price instead of deriving one (e.g. a proposed discount): the result reports the resulting margin and whether it breaches the floor.", 0),
	}
}

type pricingArgs struct {
	RuleName string `json:"rule_name"`
	financeengine.Input
}

func linesSchema() map[string]any {
	return map[string]any{
		"type": "array", "minItems": 1, "maxItems": financeengine.MaxDocumentLines, "description": "Document lines.",
		"items": object("One line.", map[string]any{
			"description":      str("What is being sold or bought"),
			"quantity":         integer("Quantity", 1),
			"unit_price_minor": integer("Unit price in minor units (net of tax unless the business prices include tax)", 0),
		}, "description", "quantity", "unit_price_minor"),
	}
}

type docArgs struct {
	Counterparty   string `json:"counterparty"`
	Lines          []line `json:"lines"`
	TaxCode        string `json:"tax_code"`
	DueInDays      int32  `json:"due_in_days"`
	Reference      string `json:"reference"`
	IdempotencyKey string `json:"idempotency_key"`
}

type line struct {
	Description    string `json:"description"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
}

func (a docArgs) draft() *agentv1.DocumentDraft {
	d := &agentv1.DocumentDraft{Counterparty: a.Counterparty, TaxCode: a.TaxCode, DueInDays: a.DueInDays, Reference: a.Reference, IdempotencyKey: a.IdempotencyKey}
	for _, l := range a.Lines {
		d.Lines = append(d.Lines, &agentv1.DocumentLine{Description: l.Description, Quantity: l.Quantity, UnitPriceMinor: l.UnitPriceMinor})
	}
	return d
}

func docSchema(counterpartyDesc string) string {
	return schemaOf(map[string]any{
		"counterparty":    str(counterpartyDesc),
		"lines":           linesSchema(),
		"tax_code":        str("A tax rate code configured for this business (see the knowledge in your skill; an unknown code is rejected with the valid list)."),
		"due_in_days":     integer("Payment term in days (0-365). Drives the cash-flow forecast.", 0),
		"reference":       str("Optional external reference (PO number, order id)."),
		"idempotency_key": str(idemDesc),
	}, "counterparty", "lines", "tax_code", "due_in_days", "idempotency_key")
}

func queuedMessage(id string) string {
	return fmt.Sprintf("Queued for the founder's approval as %s. NOTHING has been sent, posted or paid yet. "+
		"Do not proceed as if it were approved; check back with get_approval_status.", id)
}

func rawJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// ───────────────────────────── the tool set ─────────────────────────────

func standardTools() []tool {
	return []tool{
		{
			Name: "list_pricing_rules", Family: domain.ToolERPPricing, ReadOnly: true,
			Description: "List this business's active pricing rules: model, currency, margin floor and target, the approval threshold, and the formula that says which inputs each rule needs. Call this before calculate_price.",
			Schema:      schemaOf(map[string]any{}),
			run: typed(func(ctx context.Context, c *call, _ struct{}) (any, error) {
				r, err := c.agent.ListPricingRules(ctx, authed(c, &agentv1.ListPricingRulesRequest{}))
				if err != nil {
					return nil, err
				}
				type rule struct {
					Name                   string `json:"name"`
					Model                  string `json:"model"`
					Currency               string `json:"currency"`
					MarginFloorBps         int32  `json:"margin_floor_bps"`
					MarginTargetBps        int32  `json:"margin_target_bps,omitempty"`
					ApprovalThresholdMinor int64  `json:"approval_threshold_minor,omitempty"`
					Formula                any    `json:"formula"`
				}
				out := make([]rule, 0, len(r.Msg.Rules))
				for _, x := range r.Msg.Rules {
					out = append(out, rule{x.Name, x.Model, x.Currency, x.MarginFloorBps, x.MarginTargetBps, x.ApprovalThresholdMinor, rawJSON(x.FormulaJson)})
				}
				return map[string]any{"rules": out}, nil
			}),
		},
		{
			Name: "calculate_price", Family: domain.ToolERPPricing, ReadOnly: true,
			Description: "Price an offer deterministically under a pricing rule: cost build-up, price, margin, whether it breaches the margin floor, and whether it needs founder approval. This is the ONLY source of prices and margins; never compute them yourself. Money is in minor units.",
			Schema:      schemaOf(pricingProps(), "rule_name"),
			run: typed(func(ctx context.Context, c *call, a pricingArgs) (any, error) {
				in, _ := json.Marshal(a.Input)
				r, err := c.agent.CalculatePrice(ctx, authed(c, &agentv1.CalculatePriceRequest{RuleName: a.RuleName, InputJson: in}))
				if err != nil {
					return nil, err
				}
				return rawJSON(r.Msg.ResultJson), nil
			}),
		},
		{
			Name: "forecast_cash_flow", Family: domain.ToolERPPricing, ReadOnly: true,
			Description: "Project this business's cash position day by day from the ledger balance and scheduled receipts and payments: weekly buckets, lowest point, and runway (days until cash would go negative). Hoped-for (FORECAST) inflows are excluded unless asked for.",
			Schema: schemaOf(map[string]any{
				"days":                     integer("Horizon in days, 1-366 (default 90).", 1),
				"bucket_days":              integer("Reporting bucket width in days (default 7).", 1),
				"include_forecast_inflows": boolean("Also count inflows that are not yet committed. Default false."),
			}),
			run: typed(func(ctx context.Context, c *call, a struct {
				Days                   int32 `json:"days"`
				BucketDays             int32 `json:"bucket_days"`
				IncludeForecastInflows bool  `json:"include_forecast_inflows"`
			}) (any, error) {
				r, err := c.agent.ForecastCashFlow(ctx, authed(c, &agentv1.ForecastCashFlowRequest{Days: a.Days, BucketDays: a.BucketDays, IncludeForecastInflows: a.IncludeForecastInflows}))
				if err != nil {
					return nil, err
				}
				return map[string]any{"currency": r.Msg.Currency, "forecast": rawJSON(r.Msg.ForecastJson)}, nil
			}),
		},
		{
			Name: "create_quote", Family: domain.ToolERPPricing,
			Description: "Price an offer and queue the quote for the founder's approval. The numbers are computed by the pricing engine from your inputs; you cannot set them. Nothing is sent to the customer until the founder approves.",
			Schema: schemaOf(mergeProps(pricingProps(), map[string]any{
				"customer":        str("Customer name"),
				"valid_days":      integer("How long the quote stays valid once released, 1-90 (default 14).", 1),
				"note":            str("Optional note for the founder."),
				"idempotency_key": str(idemDesc),
			}), "rule_name", "customer", "idempotency_key"),
			run: typed(func(ctx context.Context, c *call, a struct {
				pricingArgs
				Customer       string `json:"customer"`
				ValidDays      int32  `json:"valid_days"`
				Note           string `json:"note"`
				IdempotencyKey string `json:"idempotency_key"`
			}) (any, error) {
				in, _ := json.Marshal(a.Input)
				r, err := c.agent.CreateQuote(ctx, authed(c, &agentv1.CreateQuoteRequest{
					RuleName: a.RuleName, InputJson: in, Customer: a.Customer, ValidDays: a.ValidDays, Note: a.Note, IdempotencyKey: a.IdempotencyKey}))
				if err != nil {
					return nil, err
				}
				d := r.Msg.Document
				return map[string]any{"approval_id": d.ApprovalId, "status": d.Status, "created": d.Created, "quote": rawJSON(d.ComputedJson), "message": queuedMessage(d.ApprovalId)}, nil
			}),
		},
		{
			Name: "draft_invoice", Family: domain.ToolERPPricing,
			Description: "Compute an invoice (line totals, tax from the business's tax rates, grand total) and queue it for the founder's approval. When approved it is posted to the ledger and the receipt is scheduled in the cash-flow forecast. Nothing is posted or sent until then.",
			Schema:      docSchema("Customer name"),
			run: typed(func(ctx context.Context, c *call, a docArgs) (any, error) {
				r, err := c.agent.DraftInvoice(ctx, authed(c, &agentv1.DraftInvoiceRequest{Draft: a.draft()}))
				if err != nil {
					return nil, err
				}
				d := r.Msg.Document
				return map[string]any{"approval_id": d.ApprovalId, "status": d.Status, "created": d.Created, "totals": rawJSON(d.ComputedJson), "message": queuedMessage(d.ApprovalId)}, nil
			}),
		},
		{
			Name: "draft_purchase_order", Family: domain.ToolERPPricing,
			Description: "Compute a purchase order and queue it for the founder's approval. When approved, the payment is scheduled in the cash-flow forecast. Nothing is sent to the supplier until then.",
			Schema:      docSchema("Supplier name"),
			run: typed(func(ctx context.Context, c *call, a docArgs) (any, error) {
				r, err := c.agent.DraftPurchaseOrder(ctx, authed(c, &agentv1.DraftPurchaseOrderRequest{Draft: a.draft()}))
				if err != nil {
					return nil, err
				}
				d := r.Msg.Document
				return map[string]any{"approval_id": d.ApprovalId, "status": d.Status, "created": d.Created, "totals": rawJSON(d.ComputedJson), "message": queuedMessage(d.ApprovalId)}, nil
			}),
		},
		{
			Name: "request_human_approval", Family: domain.ToolGovernance,
			Description: "Ask the founder to approve something that is outward-facing or irreversible and is not a quote, invoice or purchase order (those have their own tools): a payment, a bulk campaign, a change to an agent skill, or anything else. Describe exactly what will happen if approved.",
			Schema: schemaOf(map[string]any{
				"kind":            enum("What kind of request", "PAYMENT", "CAMPAIGN", "AGENT_SKILL", "OTHER"),
				"title":           str("Short headline (max 200 chars)"),
				"summary":         str("What will happen if approved, and why (max 2000 chars)"),
				"payload":         map[string]any{"type": "object", "description": "Structured details the founder needs (payee, audience, text, ...). Max 32 KB."},
				"amount_minor":    integer("Amount involved in minor units. Required for PAYMENT.", 0),
				"currency":        str("3-letter uppercase currency code. Required when amount_minor is set."),
				"risk_level":      enum("Your assessment (PAYMENT is always treated as HIGH)", "LOW", "MEDIUM", "HIGH"),
				"idempotency_key": str(idemDesc),
			}, "kind", "title", "idempotency_key"),
			run: typed(func(ctx context.Context, c *call, a struct {
				Kind           string          `json:"kind"`
				Title          string          `json:"title"`
				Summary        string          `json:"summary"`
				Payload        json.RawMessage `json:"payload"`
				AmountMinor    int64           `json:"amount_minor"`
				Currency       string          `json:"currency"`
				RiskLevel      string          `json:"risk_level"`
				IdempotencyKey string          `json:"idempotency_key"`
			}) (any, error) {
				r, err := c.agent.RequestApproval(ctx, authed(c, &agentv1.RequestApprovalRequest{
					Kind: a.Kind, Title: a.Title, Summary: a.Summary, PayloadJson: a.Payload, AmountMinor: a.AmountMinor,
					Currency: a.Currency, RiskLevel: a.RiskLevel, IdempotencyKey: a.IdempotencyKey}))
				if err != nil {
					return nil, err
				}
				x := r.Msg.Approval
				return map[string]any{"approval_id": x.ApprovalId, "status": x.Status, "created": x.Created, "message": queuedMessage(x.ApprovalId)}, nil
			}),
		},
		{
			Name: "escalate_blocker", Family: domain.ToolGovernance,
			Description: "Tell the founder you cannot proceed: missing data, a request outside your role, or a conflict you should not resolve yourself. Say what you tried.",
			Schema: schemaOf(map[string]any{
				"title":           str("What is blocked (max 200 chars)"),
				"details":         str("What you tried and what you need (max 4000 chars)"),
				"severity":        enum("How urgent", "LOW", "MEDIUM", "HIGH"),
				"idempotency_key": str(idemDesc),
			}, "title", "details", "idempotency_key"),
			run: typed(func(ctx context.Context, c *call, a struct {
				Title          string `json:"title"`
				Details        string `json:"details"`
				Severity       string `json:"severity"`
				IdempotencyKey string `json:"idempotency_key"`
			}) (any, error) {
				r, err := c.agent.EscalateBlocker(ctx, authed(c, &agentv1.EscalateBlockerRequest{Title: a.Title, Details: a.Details, Severity: a.Severity, IdempotencyKey: a.IdempotencyKey}))
				if err != nil {
					return nil, err
				}
				x := r.Msg.Approval
				return map[string]any{"escalation_id": x.ApprovalId, "status": x.Status, "created": x.Created, "message": "The founder has been notified. Do not retry the blocked work until they respond."}, nil
			}),
		},
		{
			Name: "get_approval_status", Family: domain.ToolGovernance, ReadOnly: true,
			Description: "Check a request you queued: PENDING_APPROVAL, APPROVED, REJECTED (with the founder's note), EXECUTING, EXECUTED or FAILED. Only act on it once it is APPROVED or EXECUTED.",
			Schema:      schemaOf(map[string]any{"approval_id": str("The approval_id returned when you queued the request.")}, "approval_id"),
			run: typed(func(ctx context.Context, c *call, a struct {
				ApprovalID string `json:"approval_id"`
			}) (any, error) {
				r, err := c.agent.GetApprovalStatus(ctx, authed(c, &agentv1.GetApprovalStatusRequest{ApprovalId: a.ApprovalID}))
				if err != nil {
					return nil, err
				}
				x := r.Msg.Approval
				return map[string]any{"approval_id": x.ApprovalId, "status": x.Status, "decision_note": x.DecisionNote}, nil
			}),
		},
	}
}

func mergeProps(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
