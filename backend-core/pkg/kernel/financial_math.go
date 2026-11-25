package kernel

import (
	"errors"

	"github.com/shopspring/decimal"
)

var (
	ErrNegativeAmount = errors.New("monetary amount cannot be negative")
)

type LineItem struct {
	SKU       string          `json:"sku"`
	Quantity  int32           `json:"quantity"`
	UnitPrice decimal.Decimal `json:"unit_price"`
	Discount  decimal.Decimal `json:"discount"`
}

type OrderTotals struct {
	Subtotal   decimal.Decimal `json:"subtotal"`
	Discount   decimal.Decimal `json:"discount"`
	TaxAmount  decimal.Decimal `json:"tax_amount"`
	GrandTotal decimal.Decimal `json:"grand_total"`
}

// CalculateOrderTotals performs exact financial arithmetic with decimal precision.
// taxRate: e.g. 0.18 for 18% GST
func CalculateOrderTotals(items []LineItem, taxRate decimal.Decimal) (OrderTotals, error) {
	subtotal := decimal.Zero
	totalDiscount := decimal.Zero

	for _, item := range items {
		if item.UnitPrice.IsNegative() {
			return OrderTotals{}, ErrNegativeAmount
		}
		itemTotal := item.UnitPrice.Mul(decimal.NewFromInt32(item.Quantity))
		subtotal = subtotal.Add(itemTotal)
		totalDiscount = totalDiscount.Add(item.Discount)
	}

	taxableAmount := subtotal.Sub(totalDiscount)
	if taxableAmount.IsNegative() {
		taxableAmount = decimal.Zero
	}

	// 2 decimal places banking rounding (RoundBankers)
	taxAmount := taxableAmount.Mul(taxRate).RoundBank(2)
	grandTotal := taxableAmount.Add(taxAmount).RoundBank(2)

	return OrderTotals{
		Subtotal:   subtotal.RoundBank(2),
		Discount:   totalDiscount.RoundBank(2),
		TaxAmount:  taxAmount,
		GrandTotal: grandTotal,
	}, nil
}
