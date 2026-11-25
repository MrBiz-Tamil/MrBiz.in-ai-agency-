package financeengine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mrbiz/venture-os/internal/domain"
)

// Rule is a tenant pricing rule as stored in tenant_pricing_rules.
type Rule struct {
	Name                   string
	Model                  domain.PricingModel
	Currency               string
	Formula                json.RawMessage
	MarginFloorBps         int
	MarginTargetBps        *int
	ApprovalThresholdMinor *int64
}

// Input is a pricing request. Exactly the section matching the rule's model must be set.
type Input struct {
	SaaS *SaaSInput `json:"saas,omitempty"`
	D2C  *D2CInput  `json:"d2c,omitempty"`
	EXIM *EXIMInput `json:"exim,omitempty"`
	// TargetMarginBps overrides the rule's target margin for cost-plus models (D2C, EXIM).
	TargetMarginBps *int `json:"target_margin_bps,omitempty"`
	// PriceMinor evaluates a price chosen by someone else (e.g. a proposed discount) instead of deriving one: the
	// result reports the margin it would yield and whether it breaches the floor.
	PriceMinor *int64 `json:"price_minor,omitempty"`
}

// Component is one line of the cost build-up.
type Component struct {
	Name        string `json:"name"`
	AmountMinor int64  `json:"amount_minor"`
}

// Result is a priced quote. Every number an agent may quote is in here.
type Result struct {
	Rule             string      `json:"rule"`
	Model            string      `json:"model"`
	Currency         string      `json:"currency"`
	Components       []Component `json:"components"`
	CostMinor        int64       `json:"cost_minor"`
	PriceMinor       int64       `json:"price_minor"`
	Quantity         int64       `json:"quantity"`
	UnitPriceMinor   int64       `json:"unit_price_minor"`
	MarginBps        int         `json:"margin_bps"`
	FloorBps         int         `json:"margin_floor_bps"`
	BelowFloor       bool        `json:"below_floor"`
	RequiresApproval bool        `json:"requires_approval"`
	ApprovalReasons  []string    `json:"approval_reasons,omitempty"`
	Notes            []string    `json:"notes,omitempty"`
}

// Price computes a quote under rule for in. It validates strictly: unknown formula keys, missing or negative inputs
// and impossible margins are errors, never silent defaults.
func Price(rule Rule, in Input) (Result, error) {
	if err := checkBps("margin_floor_bps", rule.MarginFloorBps); err != nil {
		return Result{}, err
	}
	if in.PriceMinor != nil {
		if err := checkAmount("price_minor", *in.PriceMinor); err != nil {
			return Result{}, err
		}
	}
	target := rule.MarginFloorBps
	if rule.MarginTargetBps != nil {
		target = *rule.MarginTargetBps
	}
	if in.TargetMarginBps != nil {
		target = *in.TargetMarginBps
	}
	if err := checkBps("target_margin_bps", target); err != nil {
		return Result{}, err
	}

	var (
		comps []Component
		cost  int64
		price int64
		qty   int64
		notes []string
		err   error
	)
	switch rule.Model {
	case domain.PricingSaaSTier:
		if in.SaaS == nil {
			return Result{}, invalid("rule %q is a SAAS_TIER rule: provide the \"saas\" section", rule.Name)
		}
		comps, cost, price, qty, notes, err = priceSaaS(rule, *in.SaaS, in.PriceMinor)
	case domain.PricingD2CUnit:
		if in.D2C == nil {
			return Result{}, invalid("rule %q is a D2C_UNIT rule: provide the \"d2c\" section", rule.Name)
		}
		comps, cost, price, qty, notes, err = priceD2C(rule, *in.D2C, target, in.PriceMinor)
	case domain.PricingEXIMLanded:
		if in.EXIM == nil {
			return Result{}, invalid("rule %q is an EXIM_LANDED_COST rule: provide the \"exim\" section", rule.Name)
		}
		comps, cost, price, qty, notes, err = priceEXIM(rule, *in.EXIM, target, in.PriceMinor)
	default:
		return Result{}, invalid("pricing model %q has no deterministic calculator; use a typed model", rule.Model)
	}
	if err != nil {
		return Result{}, err
	}

	res := Result{
		Rule: rule.Name, Model: string(rule.Model), Currency: rule.Currency,
		Components: comps, CostMinor: cost, PriceMinor: price, Quantity: qty,
		MarginBps: marginBps(price, cost), FloorBps: rule.MarginFloorBps,
		BelowFloor: belowFloor(price, cost, rule.MarginFloorBps), Notes: notes,
	}
	if qty > 0 {
		res.UnitPriceMinor, err = mulDivCeil(price, 1, qty)
		if err != nil {
			return Result{}, err
		}
	}
	if res.BelowFloor {
		res.RequiresApproval = true
		res.ApprovalReasons = append(res.ApprovalReasons,
			fmt.Sprintf("margin %s is below the floor %s", pct(res.MarginBps), pct(rule.MarginFloorBps)))
	}
	if rule.ApprovalThresholdMinor != nil && price >= *rule.ApprovalThresholdMinor {
		res.RequiresApproval = true
		res.ApprovalReasons = append(res.ApprovalReasons,
			fmt.Sprintf("price %s meets the approval threshold %s", FormatMinor(price, rule.Currency), FormatMinor(*rule.ApprovalThresholdMinor, rule.Currency)))
	}
	return res, nil
}

func pct(bps int) string {
	sign := ""
	if bps < 0 {
		sign, bps = "-", -bps
	}
	return fmt.Sprintf("%s%d.%02d%%", sign, bps/100, bps%100)
}

// decodeFormula decodes a rule's formula strictly: an unknown key is a configuration error, not something to skip.
func decodeFormula(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return invalid("pricing formula: %v", err)
	}
	return nil
}

// ───────────────────────────── SaaS tiers ─────────────────────────────

// SaaSInput selects a tier and billing term.
type SaaSInput struct {
	Tier    string `json:"tier"`
	Seats   int64  `json:"seats"`
	Billing string `json:"billing"` // "monthly" | "annual"
}

type saasFormula struct {
	Tiers []struct {
		Name             string `json:"name"`
		MinSeats         int64  `json:"min_seats"`
		MaxSeats         int64  `json:"max_seats"` // 0 = unbounded
		PricePerSeatMin  int64  `json:"price_per_seat_minor"`
		CostPerSeatMin   int64  `json:"cost_per_seat_minor"`
		FixedMonthlyCost int64  `json:"fixed_monthly_cost_minor"`
	} `json:"tiers"`
	AnnualDiscountBps int `json:"annual_discount_bps"`
}

func priceSaaS(rule Rule, in SaaSInput, override *int64) (comps []Component, cost, price, qty int64, notes []string, err error) {
	var f saasFormula
	if err = decodeFormula(rule.Formula, &f); err != nil {
		return
	}
	if err = checkBps("annual_discount_bps", f.AnnualDiscountBps); err != nil {
		return
	}
	if in.Seats <= 0 {
		err = invalid("seats must be at least 1")
		return
	}
	var months int64
	switch in.Billing {
	case "monthly":
		months = 1
	case "annual":
		months = 12
	default:
		err = invalid("billing must be \"monthly\" or \"annual\"")
		return
	}
	idx := -1
	var names []string
	for i, t := range f.Tiers {
		names = append(names, t.Name)
		if t.Name == in.Tier {
			idx = i
		}
	}
	if idx < 0 {
		sort.Strings(names)
		err = invalid("unknown tier %q (available: %v)", in.Tier, names)
		return
	}
	t := f.Tiers[idx]
	if in.Seats < t.MinSeats || (t.MaxSeats > 0 && in.Seats > t.MaxSeats) {
		err = invalid("tier %q covers %d to %d seats (0 = unbounded), requested %d", t.Name, t.MinSeats, t.MaxSeats, in.Seats)
		return
	}
	for name, v := range map[string]int64{"price_per_seat_minor": t.PricePerSeatMin, "cost_per_seat_minor": t.CostPerSeatMin, "fixed_monthly_cost_minor": t.FixedMonthlyCost} {
		if err = checkAmount(name, v); err != nil {
			return
		}
	}

	monthlyList, e := mul(t.PricePerSeatMin, in.Seats)
	if e != nil {
		err = e
		return
	}
	list, e := mul(monthlyList, months)
	if e != nil {
		err = e
		return
	}
	if months == 12 && f.AnnualDiscountBps > 0 {
		disc, e := bps(list, f.AnnualDiscountBps)
		if e != nil {
			err = e
			return
		}
		list -= disc
		notes = append(notes, fmt.Sprintf("annual billing includes a %s discount", pct(f.AnnualDiscountBps)))
	}
	seatCost, e := mul(t.CostPerSeatMin, in.Seats)
	if e != nil {
		err = e
		return
	}
	monthlyCost, e := add(seatCost, t.FixedMonthlyCost)
	if e != nil {
		err = e
		return
	}
	cost, e = mul(monthlyCost, months)
	if e != nil {
		err = e
		return
	}
	seatTotal, e1 := mul(seatCost, months)
	fixedTotal, e2 := mul(t.FixedMonthlyCost, months)
	if err = firstErr(e1, e2); err != nil {
		return
	}
	comps = []Component{
		{Name: fmt.Sprintf("seat cost x %d seats x %d months", in.Seats, months), AmountMinor: seatTotal},
		{Name: fmt.Sprintf("fixed platform cost x %d months", months), AmountMinor: fixedTotal},
	}
	price = list
	if override != nil {
		price = *override
		notes = append(notes, "price supplied by caller; the tier list price is not used")
	}
	return comps, cost, price, in.Seats, notes, nil
}

// ───────────────────────────── D2C unit economics ─────────────────────────────

// D2CInput is one order of Quantity units.
type D2CInput struct {
	Quantity int64 `json:"quantity"`
}

type d2cFormula struct {
	UnitCostMinor       int64 `json:"unit_cost_minor"`
	PackagingMinor      int64 `json:"packaging_minor_per_unit"`
	ShippingMinor       int64 `json:"shipping_minor_per_order"`
	CACMinor            int64 `json:"cac_minor_per_order"`
	PaymentFeeBps       int   `json:"payment_fee_bps"`
	PaymentFeeFixed     int64 `json:"payment_fee_fixed_minor"`
	ReturnsProvisionBps int   `json:"returns_provision_bps"`
}

func priceD2C(rule Rule, in D2CInput, target int, override *int64) (comps []Component, cost, price, qty int64, notes []string, err error) {
	var f d2cFormula
	if err = decodeFormula(rule.Formula, &f); err != nil {
		return
	}
	if in.Quantity <= 0 {
		err = invalid("quantity must be at least 1")
		return
	}
	for name, v := range map[string]int64{"unit_cost_minor": f.UnitCostMinor, "packaging_minor_per_unit": f.PackagingMinor,
		"shipping_minor_per_order": f.ShippingMinor, "cac_minor_per_order": f.CACMinor, "payment_fee_fixed_minor": f.PaymentFeeFixed} {
		if err = checkAmount(name, v); err != nil {
			return
		}
	}
	if err = checkBps("payment_fee_bps", f.PaymentFeeBps); err != nil {
		return
	}
	if err = checkBps("returns_provision_bps", f.ReturnsProvisionBps); err != nil {
		return
	}

	goods, e1 := mul(f.UnitCostMinor, in.Quantity)
	pack, e2 := mul(f.PackagingMinor, in.Quantity)
	if err = firstErr(e1, e2); err != nil {
		return
	}
	// Costs that do not depend on the price.
	fixed, e := add(goods, pack, f.ShippingMinor, f.CACMinor, f.PaymentFeeFixed)
	if e != nil {
		err = e
		return
	}

	// costAt is the full cost of selling at price p: fixed costs plus the rounded price-proportional ones.
	costAt := func(p int64) (c []Component, total int64, e error) {
		fee, e1 := bps(p, f.PaymentFeeBps)
		ret, e2 := bps(p, f.ReturnsProvisionBps)
		if e = firstErr(e1, e2); e != nil {
			return
		}
		c = []Component{
			{Name: "goods", AmountMinor: goods},
			{Name: "packaging", AmountMinor: pack},
			{Name: "shipping", AmountMinor: f.ShippingMinor},
			{Name: "customer acquisition", AmountMinor: f.CACMinor},
			{Name: "payment fees", AmountMinor: fee + f.PaymentFeeFixed},
			{Name: "returns provision", AmountMinor: ret},
		}
		total, e = add(goods, pack, f.ShippingMinor, f.CACMinor, fee, f.PaymentFeeFixed, ret)
		return
	}

	if override != nil {
		price = *override
		notes = append(notes, "price supplied by caller; margin is evaluated, not derived")
	} else {
		// price*(1 - fee - returns - target) >= fixed  =>  price = ceil(fixed / (1 - fee - returns - target)).
		headroom := 10000 - f.PaymentFeeBps - f.ReturnsProvisionBps - target
		if headroom <= 0 {
			err = invalid("no price can reach a %s margin: payment fee %s and returns provision %s leave no headroom",
				pct(target), pct(f.PaymentFeeBps), pct(f.ReturnsProvisionBps))
			return
		}
		if price, err = mulDivCeil(fixed, 10000, int64(headroom)); err != nil {
			return
		}
		// The closed form ignores that fees are rounded to whole minor units, which can leave the margin a hair
		// under target. Verify with the real rounded costs and step up until it holds (at most a few units).
		for n := 0; ; n++ {
			var total int64
			if _, total, err = costAt(price); err != nil {
				return
			}
			if !belowFloor(price, total, target) {
				break
			}
			if n >= 64 {
				err = invalid("could not converge on a price that meets a %s margin", pct(target))
				return
			}
			price++
		}
	}
	comps, cost, err = costAt(price)
	return comps, cost, price, in.Quantity, notes, err
}

// ───────────────────────────── export / import landed cost ─────────────────────────────

// EXIMInput carries the shipment facts. Every amount is a total for the shipment (not per unit), in the rule's
// currency. Components that the chosen Incoterm needs are REQUIRED (a nil pointer is an error, not zero).
type EXIMInput struct {
	Incoterm           string `json:"incoterm"` // FOB | CIF | DDP
	Quantity           int64  `json:"quantity"`
	UnitCostMinor      int64  `json:"unit_cost_minor"`
	PackingMinor       int64  `json:"packing_minor"`
	InlandFreightMinor int64  `json:"inland_freight_minor"`
	OceanFreightMinor  *int64 `json:"ocean_freight_minor,omitempty"`       // CIF, DDP
	DestinationMinor   *int64 `json:"destination_charges_minor,omitempty"` // DDP: port, clearance, last mile
}

type eximFormula struct {
	Incoterms           []string `json:"incoterms"`
	Components          []string `json:"components"` // documentation only
	FXBufferBps         int      `json:"fx_buffer_bps"`
	ExportHandlingMinor int64    `json:"export_handling_minor"`
	InsuranceBps        int      `json:"insurance_bps"`
	DutyBps             int      `json:"duty_bps"`
}

func priceEXIM(rule Rule, in EXIMInput, target int, override *int64) (comps []Component, cost, price, qty int64, notes []string, err error) {
	var f eximFormula
	if err = decodeFormula(rule.Formula, &f); err != nil {
		return
	}
	allowed := false
	for _, t := range f.Incoterms {
		allowed = allowed || t == in.Incoterm
	}
	if !allowed {
		err = invalid("incoterm %q is not enabled for rule %q (enabled: %v)", in.Incoterm, rule.Name, f.Incoterms)
		return
	}
	if in.Quantity <= 0 {
		err = invalid("quantity must be at least 1")
		return
	}
	for name, v := range map[string]int64{"unit_cost_minor": in.UnitCostMinor, "packing_minor": in.PackingMinor,
		"inland_freight_minor": in.InlandFreightMinor, "export_handling_minor": f.ExportHandlingMinor} {
		if err = checkAmount(name, v); err != nil {
			return
		}
	}
	for name, v := range map[string]int{"fx_buffer_bps": f.FXBufferBps, "insurance_bps": f.InsuranceBps, "duty_bps": f.DutyBps} {
		if err = checkBps(name, v); err != nil {
			return
		}
	}

	needOcean := in.Incoterm == "CIF" || in.Incoterm == "DDP"
	needDest := in.Incoterm == "DDP"
	if needOcean && in.OceanFreightMinor == nil {
		err = invalid("ocean_freight_minor is required for %s", in.Incoterm)
		return
	}
	if needDest && in.DestinationMinor == nil {
		err = invalid("destination_charges_minor is required for DDP")
		return
	}

	goods, e := mul(in.UnitCostMinor, in.Quantity)
	if e != nil {
		err = e
		return
	}
	fxBuf, e := bps(goods, f.FXBufferBps)
	if e != nil {
		err = e
		return
	}
	comps = []Component{
		{Name: "goods", AmountMinor: goods},
		{Name: "currency buffer", AmountMinor: fxBuf},
		{Name: "packing", AmountMinor: in.PackingMinor},
		{Name: "inland freight", AmountMinor: in.InlandFreightMinor},
		{Name: "export handling", AmountMinor: f.ExportHandlingMinor},
	}
	fob, e := add(goods, fxBuf, in.PackingMinor, in.InlandFreightMinor, f.ExportHandlingMinor)
	if e != nil {
		err = e
		return
	}
	cost = fob
	if needOcean {
		if err = checkAmount("ocean_freight_minor", *in.OceanFreightMinor); err != nil {
			return
		}
		ins, e := bps(fob+*in.OceanFreightMinor, f.InsuranceBps)
		if e != nil {
			err = e
			return
		}
		comps = append(comps, Component{Name: "ocean freight", AmountMinor: *in.OceanFreightMinor}, Component{Name: "insurance", AmountMinor: ins})
		if cost, err = add(cost, *in.OceanFreightMinor, ins); err != nil {
			return
		}
	}
	if needDest {
		if err = checkAmount("destination_charges_minor", *in.DestinationMinor); err != nil {
			return
		}
		cifBasis := cost
		duty, e := bps(cifBasis, f.DutyBps)
		if e != nil {
			err = e
			return
		}
		comps = append(comps, Component{Name: "import duty", AmountMinor: duty}, Component{Name: "destination charges", AmountMinor: *in.DestinationMinor})
		if cost, err = add(cost, duty, *in.DestinationMinor); err != nil {
			return
		}
		notes = append(notes, "import duty is computed on the CIF cost basis; confirm the customs value with your broker")
	}

	if override != nil {
		price = *override
		notes = append(notes, "price supplied by caller; margin is evaluated, not derived")
	} else {
		if target >= 10000 {
			err = invalid("a 100%% margin has no finite price")
			return
		}
		if price, err = mulDivCeil(cost, 10000, int64(10000-target)); err != nil {
			return
		}
	}
	notes = append(notes, fmt.Sprintf("%s landed cost for the whole shipment", in.Incoterm))
	return comps, cost, price, in.Quantity, notes, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
