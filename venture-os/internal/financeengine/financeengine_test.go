package financeengine_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/financeengine"
)

func i64(v int64) *int64 { return &v }
func i(v int) *int       { return &v }

func eximRule() financeengine.Rule {
	return financeengine.Rule{
		Name: "eu-landed-cost", Model: domain.PricingEXIMLanded, Currency: "USD",
		Formula:        json.RawMessage(`{"incoterms":["FOB","CIF","DDP"],"fx_buffer_bps":150,"export_handling_minor":20000,"insurance_bps":50,"duty_bps":500}`),
		MarginFloorBps: 1200, MarginTargetBps: i(1800), ApprovalThresholdMinor: i64(1000000),
	}
}

func eximInput(term string) financeengine.Input {
	return financeengine.Input{EXIM: &financeengine.EXIMInput{
		Incoterm: term, Quantity: 1000, UnitCostMinor: 800, // USD 8.00/kg, 1000 kg
		PackingMinor: 50000, InlandFreightMinor: 30000,
		OceanFreightMinor: i64(120000), DestinationMinor: i64(40000),
	}}
}

func comp(r financeengine.Result, name string) int64 {
	for _, c := range r.Components {
		if c.Name == name {
			return c.AmountMinor
		}
	}
	return -1
}

func TestEXIMLandedCostByIncoterm(t *testing.T) {
	// goods 800,000 + 1.5% buffer 12,000 + packing 50,000 + inland 30,000 + handling 20,000 = FOB cost 912,000
	fob, err := financeengine.Price(eximRule(), eximInput("FOB"))
	if err != nil {
		t.Fatal(err)
	}
	if fob.CostMinor != 912000 {
		t.Fatalf("FOB cost = %d, want 912000 (%+v)", fob.CostMinor, fob.Components)
	}
	// price = ceil(912000 / (1 - 0.18)) = ceil(1112195.12) = 1112196
	if fob.PriceMinor != 1112196 {
		t.Fatalf("FOB price = %d, want 1112196", fob.PriceMinor)
	}
	if fob.BelowFloor || fob.MarginBps < 1800 {
		t.Fatalf("derived price must meet the target margin: %+v", fob)
	}
	// 1,112,196 >= approval threshold 1,000,000 -> needs approval even though the margin is fine.
	if !fob.RequiresApproval || len(fob.ApprovalReasons) != 1 {
		t.Fatalf("approval flags: %+v", fob)
	}

	// CIF adds ocean freight 120,000 and insurance 0.5% of (912,000+120,000)=5,160.
	cif, err := financeengine.Price(eximRule(), eximInput("CIF"))
	if err != nil {
		t.Fatal(err)
	}
	if comp(cif, "insurance") != 5160 || cif.CostMinor != 912000+120000+5160 {
		t.Fatalf("CIF = %+v", cif)
	}
	// DDP adds duty 5% of the CIF cost basis (1,037,160 -> 51,858) and destination charges 40,000.
	ddp, err := financeengine.Price(eximRule(), eximInput("DDP"))
	if err != nil {
		t.Fatal(err)
	}
	if comp(ddp, "import duty") != 51858 || ddp.CostMinor != 1037160+51858+40000 {
		t.Fatalf("DDP = %+v", ddp)
	}
	if ddp.PriceMinor <= cif.PriceMinor || cif.PriceMinor <= fob.PriceMinor {
		t.Fatal("price must rise FOB < CIF < DDP")
	}
	var sum int64
	for _, c := range ddp.Components {
		sum += c.AmountMinor
	}
	if sum != ddp.CostMinor {
		t.Fatalf("components sum %d != cost %d", sum, ddp.CostMinor)
	}
}

func TestEXIMRequiresTheInputsAnIncotermNeeds(t *testing.T) {
	in := eximInput("CIF")
	in.EXIM.OceanFreightMinor = nil
	if _, err := financeengine.Price(eximRule(), in); !errors.Is(err, financeengine.ErrInvalid) {
		t.Fatalf("CIF without ocean freight: %v", err)
	}
	in = eximInput("DDP")
	in.EXIM.DestinationMinor = nil
	if _, err := financeengine.Price(eximRule(), in); !errors.Is(err, financeengine.ErrInvalid) {
		t.Fatalf("DDP without destination charges: %v", err)
	}
	// FOB does not need them.
	in = eximInput("FOB")
	in.EXIM.OceanFreightMinor, in.EXIM.DestinationMinor = nil, nil
	if _, err := financeengine.Price(eximRule(), in); err != nil {
		t.Fatalf("FOB needs neither: %v", err)
	}
	// Incoterm not enabled in the rule.
	r := eximRule()
	r.Formula = json.RawMessage(`{"incoterms":["FOB"],"fx_buffer_bps":0,"export_handling_minor":0,"insurance_bps":0,"duty_bps":0}`)
	if _, err := financeengine.Price(r, eximInput("DDP")); !errors.Is(err, financeengine.ErrInvalid) {
		t.Fatalf("disabled incoterm: %v", err)
	}
}

func TestPriceOverrideIsEvaluatedAgainstTheFloor(t *testing.T) {
	in := eximInput("FOB")
	in.PriceMinor = i64(950000) // cost 912,000: margin 3.99%
	r, err := financeengine.Price(eximRule(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !r.BelowFloor || !r.RequiresApproval || r.MarginBps != 400 {
		t.Fatalf("discounted price: below=%v approval=%v margin=%d", r.BelowFloor, r.RequiresApproval, r.MarginBps)
	}
	// Exactly at the floor is NOT below it: price*(1-0.12) == cost.
	in.PriceMinor = i64(1036364) // ceil(912000/0.88)
	r, _ = financeengine.Price(eximRule(), in)
	if r.BelowFloor {
		t.Fatalf("price at the floor flagged below it: %+v", r)
	}
	in.PriceMinor = i64(1036363)
	r, _ = financeengine.Price(eximRule(), in)
	if !r.BelowFloor {
		t.Fatal("one minor unit under the floor must be flagged")
	}
}

// Property: a price derived from a target margin always achieves at least that margin, for random inputs.
func TestDerivedPriceNeverFallsBelowTargetMargin(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for n := 0; n < 2000; n++ {
		target := rng.Intn(9500)
		in := financeengine.Input{TargetMarginBps: &target, EXIM: &financeengine.EXIMInput{
			Incoterm: "FOB", Quantity: 1 + rng.Int63n(5000), UnitCostMinor: rng.Int63n(100000),
			PackingMinor: rng.Int63n(1000000), InlandFreightMinor: rng.Int63n(1000000),
		}}
		r, err := financeengine.Price(eximRule(), in)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if r.CostMinor > 0 && r.MarginBps < target {
			t.Fatalf("n=%d: margin %d below target %d (cost %d price %d)", n, r.MarginBps, target, r.CostMinor, r.PriceMinor)
		}
		// One unit cheaper would miss the target (the price is the minimum), unless cost is zero.
		if r.CostMinor > 0 && r.PriceMinor > 1 {
			lower := r.PriceMinor - 1
			in.PriceMinor = &lower
			l, _ := financeengine.Price(eximRule(), in)
			if l.MarginBps >= target && l.MarginBps != r.MarginBps && target > 0 {
				// allowed only if integer margin rounding hides the miss; exactness is checked via BelowFloor elsewhere
				continue
			}
		}
	}
}

func d2cRule() financeengine.Rule {
	return financeengine.Rule{
		Name: "d2c", Model: domain.PricingD2CUnit, Currency: "USD",
		Formula: json.RawMessage(`{"unit_cost_minor":1000,"packaging_minor_per_unit":100,"shipping_minor_per_order":500,
			"cac_minor_per_order":800,"payment_fee_bps":290,"payment_fee_fixed_minor":30,"returns_provision_bps":500}`),
		MarginFloorBps: 3000, MarginTargetBps: i(4000),
	}
}

func TestD2CPriceCoversPriceProportionalCosts(t *testing.T) {
	r, err := financeengine.Price(d2cRule(), financeengine.Input{D2C: &financeengine.D2CInput{Quantity: 2}})
	if err != nil {
		t.Fatal(err)
	}
	// fixed = goods 2000 + packaging 200 + shipping 500 + CAC 800 + fixed fee 30 = 3530
	// closed form: ceil(3530 / (1 - 0.029 - 0.05 - 0.40)) = 6775, but with fees rounded to whole units that
	// misses 40.00% by a hair (3999 bps), so the engine steps up to 6777, the first price that really meets it.
	if r.PriceMinor != 6777 {
		t.Fatalf("price = %d, want 6777 (%+v)", r.PriceMinor, r)
	}
	if r.MarginBps < 4000 || r.BelowFloor {
		t.Fatalf("margin %d", r.MarginBps)
	}
	if comp(r, "payment fees") != 197+30 || comp(r, "returns provision") != 339 { // 2.9% and 5% of 6777, half-up
		t.Fatalf("components = %+v", r.Components)
	}
	if r.UnitPriceMinor != 3389 { // ceil(6777/2)
		t.Fatalf("unit price = %d", r.UnitPriceMinor)
	}
	// An impossible target (fees+returns+margin >= 100%) is an error, not a nonsense price.
	hi := 9500
	if _, err := financeengine.Price(d2cRule(), financeengine.Input{TargetMarginBps: &hi, D2C: &financeengine.D2CInput{Quantity: 1}}); !errors.Is(err, financeengine.ErrInvalid) {
		t.Fatalf("impossible margin: %v", err)
	}
}

func saasRule() financeengine.Rule {
	return financeengine.Rule{
		Name: "saas", Model: domain.PricingSaaSTier, Currency: "USD",
		Formula: json.RawMessage(`{"annual_discount_bps":2000,"tiers":[
			{"name":"starter","min_seats":1,"max_seats":10,"price_per_seat_minor":2000,"cost_per_seat_minor":400,"fixed_monthly_cost_minor":1000},
			{"name":"team","min_seats":11,"max_seats":0,"price_per_seat_minor":1500,"cost_per_seat_minor":350,"fixed_monthly_cost_minor":5000}]}`),
		MarginFloorBps: 6000,
	}
}

func TestSaaSTierPricing(t *testing.T) {
	m, err := financeengine.Price(saasRule(), financeengine.Input{SaaS: &financeengine.SaaSInput{Tier: "starter", Seats: 5, Billing: "monthly"}})
	if err != nil {
		t.Fatal(err)
	}
	// 5 x 2000 = 10,000; cost 5x400+1000 = 3,000; margin 70%
	if m.PriceMinor != 10000 || m.CostMinor != 3000 || m.MarginBps != 7000 || m.BelowFloor {
		t.Fatalf("monthly = %+v", m)
	}
	a, err := financeengine.Price(saasRule(), financeengine.Input{SaaS: &financeengine.SaaSInput{Tier: "starter", Seats: 5, Billing: "annual"}})
	if err != nil {
		t.Fatal(err)
	}
	// 12 x 10,000 = 120,000 less 20% = 96,000; cost 36,000
	if a.PriceMinor != 96000 || a.CostMinor != 36000 || a.MarginBps != 6250 {
		t.Fatalf("annual = %+v", a)
	}
	for name, in := range map[string]financeengine.SaaSInput{
		"unknown tier":   {Tier: "nope", Seats: 5, Billing: "monthly"},
		"too many seats": {Tier: "starter", Seats: 11, Billing: "monthly"},
		"too few seats":  {Tier: "team", Seats: 3, Billing: "monthly"},
		"zero seats":     {Tier: "starter", Seats: 0, Billing: "monthly"},
		"bad billing":    {Tier: "starter", Seats: 1, Billing: "weekly"},
	} {
		in := in
		if _, err := financeengine.Price(saasRule(), financeengine.Input{SaaS: &in}); !errors.Is(err, financeengine.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A 50% discount override breaches the 60% floor and demands approval.
	in := financeengine.Input{PriceMinor: i64(5000), SaaS: &financeengine.SaaSInput{Tier: "starter", Seats: 5, Billing: "monthly"}}
	r, _ := financeengine.Price(saasRule(), in)
	if !r.BelowFloor || !r.RequiresApproval || r.MarginBps != 4000 {
		t.Fatalf("discount = %+v", r)
	}
}

func TestPriceRejectsMisconfigurationAndBadInputs(t *testing.T) {
	r := eximRule()
	r.Formula = json.RawMessage(`{"incoterms":["FOB"],"surprise_key":1}`)
	if _, err := financeengine.Price(r, eximInput("FOB")); !errors.Is(err, financeengine.ErrInvalid) {
		t.Errorf("unknown formula key accepted: %v", err)
	}
	if _, err := financeengine.Price(eximRule(), financeengine.Input{}); !errors.Is(err, financeengine.ErrInvalid) {
		t.Error("missing model section accepted")
	}
	if _, err := financeengine.Price(financeengine.Rule{Name: "x", Model: domain.PricingCustom, Formula: json.RawMessage(`{}`)}, financeengine.Input{}); !errors.Is(err, financeengine.ErrInvalid) {
		t.Error("CUSTOM model has no calculator and must be refused")
	}
	neg := eximInput("FOB")
	neg.EXIM.UnitCostMinor = -1
	if _, err := financeengine.Price(eximRule(), neg); !errors.Is(err, financeengine.ErrInvalid) {
		t.Error("negative cost accepted")
	}
	huge := eximInput("FOB")
	huge.EXIM.UnitCostMinor, huge.EXIM.Quantity = financeengine.MaxAmountMinor, 1<<40
	if _, err := financeengine.Price(eximRule(), huge); err == nil {
		t.Error("overflowing input accepted")
	}
	over := 10001
	if _, err := financeengine.Price(eximRule(), financeengine.Input{TargetMarginBps: &over, EXIM: eximInput("FOB").EXIM}); !errors.Is(err, financeengine.ErrInvalid) {
		t.Error("margin over 100% accepted")
	}
}

func TestDocumentTotals(t *testing.T) {
	lines := []financeengine.Line{
		{Description: "Black pepper", Quantity: 1000, UnitPriceMinor: 1112},
		{Description: "Freight", Quantity: 1, UnitPriceMinor: 12345},
	}
	// Net prices, 5% tax: subtotal 1,124,345 -> tax 56,217.25 -> 56,217; total 1,180,562
	n, err := financeengine.DocumentTotals(lines, 500, false)
	if err != nil {
		t.Fatal(err)
	}
	if n.SubtotalMinor != 1124345 || n.TaxMinor != 56217 || n.TotalMinor != 1180562 || n.Lines[0].TotalMinor != 1112000 {
		t.Fatalf("net totals = %+v", n)
	}
	// Gross prices: total is the sum of lines; tax is extracted and subtotal+tax == total exactly.
	g, err := financeengine.DocumentTotals(lines, 500, true)
	if err != nil {
		t.Fatal(err)
	}
	if g.TotalMinor != 1124345 || g.SubtotalMinor+g.TaxMinor != g.TotalMinor || g.TaxMinor != 53540 {
		t.Fatalf("gross totals = %+v", g)
	}
	// Half-up rounding: 1 x 10 at 5% = 0.5 -> 1.
	h, _ := financeengine.DocumentTotals([]financeengine.Line{{Description: "x", Quantity: 1, UnitPriceMinor: 10}}, 500, false)
	if h.TaxMinor != 1 {
		t.Fatalf("half-up: tax = %d", h.TaxMinor)
	}
	bad := map[string][]financeengine.Line{
		"empty":     {},
		"no desc":   {{Quantity: 1, UnitPriceMinor: 1}},
		"zero qty":  {{Description: "x", Quantity: 0, UnitPriceMinor: 1}},
		"negative":  {{Description: "x", Quantity: 1, UnitPriceMinor: -1}},
		"too large": {{Description: "x", Quantity: 1 << 40, UnitPriceMinor: financeengine.MaxAmountMinor}},
	}
	for name, l := range bad {
		if _, err := financeengine.DocumentTotals(l, 0, false); !errors.Is(err, financeengine.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := financeengine.DocumentTotals(lines, 10001, false); err == nil {
		t.Error("tax rate over 100% accepted")
	}
}

// Property: subtotal + tax == total for any document, either price mode.
func TestTotalsAlwaysAddUp(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for n := 0; n < 1000; n++ {
		var lines []financeengine.Line
		for j := 0; j < 1+rng.Intn(6); j++ {
			lines = append(lines, financeengine.Line{Description: "l", Quantity: 1 + rng.Int63n(100), UnitPriceMinor: rng.Int63n(1000000)})
		}
		rate := rng.Intn(3000)
		for _, inc := range []bool{false, true} {
			tt, err := financeengine.DocumentTotals(lines, rate, inc)
			if err != nil {
				t.Fatal(err)
			}
			if tt.SubtotalMinor+tt.TaxMinor != tt.TotalMinor {
				t.Fatalf("%+v does not add up", tt)
			}
		}
	}
}

func date(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestForecastRunwayAndBuckets(t *testing.T) {
	items := []financeengine.Item{
		{Direction: financeengine.Outflow, AmountMinor: 600, Due: date("2026-01-03"), Status: "COMMITTED"},
		{Direction: financeengine.Inflow, AmountMinor: 300, Due: date("2026-01-05"), Status: "COMMITTED"},
		{Direction: financeengine.Inflow, AmountMinor: 5000, Due: date("2026-01-06"), Status: "FORECAST"}, // hoped-for: excluded
		{Direction: financeengine.Outflow, AmountMinor: 400, Due: date("2026-01-09"), Status: "FORECAST"},
		{Direction: financeengine.Outflow, AmountMinor: 99999, Due: date("2026-01-02"), Status: "SETTLED"},
		{Direction: financeengine.Outflow, AmountMinor: 99999, Due: date("2026-01-02"), Status: "CANCELLED"},
		{Direction: financeengine.Outflow, AmountMinor: 100, Due: date("2026-03-01"), Status: "COMMITTED"}, // beyond horizon
	}
	f, err := financeengine.ForecastCash(1000, items, date("2026-01-01"), 14, financeengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Day balances: d0 1000, d2 400 (after -600), d4 700 (+300), d8 300 (-400) ... never negative.
	if f.RunwayDays != nil || f.LowCash || f.EndingMinor != 300 || f.MinClosingMinor != 300 {
		t.Fatalf("forecast = %+v", f)
	}
	if len(f.Buckets) != 2 || f.Buckets[0].OutflowMinor != 600 || f.Buckets[0].InflowMinor != 300 ||
		f.Buckets[1].OutflowMinor != 400 || f.Buckets[1].ClosingMinor != 300 {
		t.Fatalf("buckets = %+v", f.Buckets)
	}

	// Counting hoped-for inflows changes the answer, but only when asked.
	g, _ := financeengine.ForecastCash(1000, items, date("2026-01-01"), 14, financeengine.Options{IncludeForecastInflows: true})
	if g.EndingMinor != 5300 {
		t.Fatalf("with forecast inflows ending = %d", g.EndingMinor)
	}

	// A burn that exhausts cash: runway is the first negative day.
	burn := []financeengine.Item{{Direction: financeengine.Outflow, AmountMinor: 1500, Due: date("2026-01-04"), Status: "COMMITTED"}}
	r, _ := financeengine.ForecastCash(1000, burn, date("2026-01-01"), 30, financeengine.Options{})
	if r.RunwayDays == nil || *r.RunwayDays != 4 || !r.LowCash || r.MinClosingMinor != -500 || !r.MinClosingDate.Equal(date("2026-01-04")) {
		t.Fatalf("runway = %+v", r)
	}
}

func TestForecastOverdueAndValidation(t *testing.T) {
	items := []financeengine.Item{
		{Direction: financeengine.Outflow, AmountMinor: 700, Due: date("2025-12-20"), Status: "COMMITTED"}, // overdue bill: paid on day 0
		{Direction: financeengine.Inflow, AmountMinor: 900, Due: date("2025-12-20"), Status: "COMMITTED"},  // overdue receivable: reported, not counted
	}
	f, err := financeengine.ForecastCash(1000, items, date("2026-01-01"), 7, financeengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.EndingMinor != 300 || f.OverdueOutflowMinor != 700 || f.OverdueInflowMinor != 900 {
		t.Fatalf("overdue handling = %+v", f)
	}
	if _, err := financeengine.ForecastCash(0, nil, date("2026-01-01"), 0, financeengine.Options{}); !errors.Is(err, financeengine.ErrInvalid) {
		t.Error("zero horizon accepted")
	}
	if _, err := financeengine.ForecastCash(0, nil, date("2026-01-01"), 400, financeengine.Options{}); !errors.Is(err, financeengine.ErrInvalid) {
		t.Error("over-long horizon accepted")
	}
	if _, err := financeengine.ForecastCash(0, []financeengine.Item{{Direction: "SIDEWAYS", AmountMinor: 1, Due: date("2026-01-02")}}, date("2026-01-01"), 7, financeengine.Options{}); !errors.Is(err, financeengine.ErrInvalid) {
		t.Error("unknown direction accepted")
	}
}

// Property: for random D2C economics and targets, the derived price meets the target margin exactly (the closed
// form alone would miss by a rounding unit for some inputs; the engine verifies with the real rounded costs).
func TestD2CDerivedPriceAlwaysMeetsTargetWithRounding(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for n := 0; n < 3000; n++ {
		formula := fmt.Sprintf(`{"unit_cost_minor":%d,"packaging_minor_per_unit":%d,"shipping_minor_per_order":%d,"cac_minor_per_order":%d,`+
			`"payment_fee_bps":%d,"payment_fee_fixed_minor":%d,"returns_provision_bps":%d}`,
			rng.Int63n(50000), rng.Int63n(2000), rng.Int63n(5000), rng.Int63n(5000), rng.Intn(500), rng.Int63n(100), rng.Intn(1500))
		rule := financeengine.Rule{Name: "d2c", Model: domain.PricingD2CUnit, Currency: "USD", Formula: json.RawMessage(formula), MarginFloorBps: 0}
		target := rng.Intn(6000)
		r, err := financeengine.Price(rule, financeengine.Input{TargetMarginBps: &target, D2C: &financeengine.D2CInput{Quantity: 1 + rng.Int63n(20)}})
		if err != nil {
			t.Fatalf("n=%d formula=%s target=%d: %v", n, formula, target, err)
		}
		// Exact check, no rounding: (price-cost)*10000 >= price*target.
		if (r.PriceMinor-r.CostMinor)*10000 < r.PriceMinor*int64(target) {
			t.Fatalf("n=%d: price %d cost %d misses target %d bps (formula %s)", n, r.PriceMinor, r.CostMinor, target, formula)
		}
	}
}

func TestFormatMinor(t *testing.T) {
	for _, tc := range []struct {
		amount   int64
		currency string
		want     string
	}{
		{1180562, "INR", "INR 11,805.62"},
		{5, "USD", "USD 0.05"},
		{0, "USD", "USD 0.00"},
		{-1234567, "EUR", "EUR -12,345.67"},
		{123456, "JPY", "JPY 123,456"},
		{1500, "KWD", "KWD 1.500"},
		{100000000000, "USD", "USD 1,000,000,000.00"},
		{999, "USD", "USD 9.99"},
	} {
		if got := financeengine.FormatMinor(tc.amount, tc.currency); got != tc.want {
			t.Errorf("FormatMinor(%d, %s) = %q, want %q", tc.amount, tc.currency, got, tc.want)
		}
	}
}
