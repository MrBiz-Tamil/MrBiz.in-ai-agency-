package financeengine

import "fmt"

// Line is one line of an invoice, quote or purchase order.
type Line struct {
	Description    string `json:"description"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
}

// LineTotal is a line with its computed total.
type LineTotal struct {
	Line
	TotalMinor int64 `json:"total_minor"`
}

// Totals is the computed money of a document.
type Totals struct {
	Lines         []LineTotal `json:"lines"`
	SubtotalMinor int64       `json:"subtotal_minor"` // net of tax
	TaxMinor      int64       `json:"tax_minor"`
	TotalMinor    int64       `json:"total_minor"` // what the counterparty pays
	TaxRateBps    int         `json:"tax_rate_bps"`
	PricesIncTax  bool        `json:"prices_include_tax"`
}

// MaxDocumentLines bounds a single document.
const MaxDocumentLines = 200

// DocumentTotals computes line totals, tax and the grand total.
//
// With pricesIncludeTax false, unit prices are net: tax = round(subtotal * rate) and total = subtotal + tax.
// With it true, unit prices are gross: total = sum of lines and tax = round(total * rate / (10000 + rate)), so
// subtotal + tax always equals the total exactly.
func DocumentTotals(lines []Line, taxRateBps int, pricesIncludeTax bool) (Totals, error) {
	if len(lines) == 0 {
		return Totals{}, invalid("a document needs at least one line")
	}
	if len(lines) > MaxDocumentLines {
		return Totals{}, invalid("a document may have at most %d lines", MaxDocumentLines)
	}
	if err := checkBps("tax_rate_bps", taxRateBps); err != nil {
		return Totals{}, err
	}
	out := Totals{TaxRateBps: taxRateBps, PricesIncTax: pricesIncludeTax, Lines: make([]LineTotal, 0, len(lines))}
	var sum int64
	for i, l := range lines {
		if l.Description == "" {
			return Totals{}, invalid("line %d has no description", i+1)
		}
		if l.Quantity <= 0 {
			return Totals{}, invalid("line %d quantity must be at least 1", i+1)
		}
		if err := checkAmount(fmt.Sprintf("line %d unit_price_minor", i+1), l.UnitPriceMinor); err != nil {
			return Totals{}, err
		}
		t, err := mul(l.Quantity, l.UnitPriceMinor)
		if err != nil {
			return Totals{}, invalid("line %d total is too large", i+1)
		}
		if sum, err = add(sum, t); err != nil || sum > MaxAmountMinor {
			return Totals{}, invalid("document total is too large")
		}
		out.Lines = append(out.Lines, LineTotal{Line: l, TotalMinor: t})
	}
	if pricesIncludeTax {
		tax, err := mulDivRound(sum, int64(taxRateBps), int64(10000+taxRateBps))
		if err != nil {
			return Totals{}, err
		}
		out.TotalMinor, out.TaxMinor, out.SubtotalMinor = sum, tax, sum-tax
		return out, nil
	}
	tax, err := bps(sum, taxRateBps)
	if err != nil {
		return Totals{}, err
	}
	out.SubtotalMinor, out.TaxMinor, out.TotalMinor = sum, tax, sum+tax
	return out, nil
}
