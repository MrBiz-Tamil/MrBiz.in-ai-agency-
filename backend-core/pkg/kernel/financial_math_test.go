package kernel

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestCalculateOrderTotals(t *testing.T) {
	items := []LineItem{
		{
			SKU:       "SKU-SHIRT-01",
			Quantity:  2,
			UnitPrice: decimal.NewFromFloat(500.00), // 1000.00
			Discount:  decimal.NewFromFloat(100.00), // 100.00 discount
		},
		{
			SKU:       "SKU-JEANS-02",
			Quantity:  1,
			UnitPrice: decimal.NewFromFloat(1200.00), // 1200.00
			Discount:  decimal.Zero,
		},
	}

	// 18% GST tax rate
	taxRate := decimal.NewFromFloat(0.18)

	totals, err := CalculateOrderTotals(items, taxRate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Subtotal: 2*500 + 1*1200 = 2200.00
	expectedSubtotal := decimal.NewFromFloat(2200.00)
	if !totals.Subtotal.Equal(expectedSubtotal) {
		t.Errorf("expected subtotal %s, got %s", expectedSubtotal, totals.Subtotal)
	}

	// Discount: 100.00
	expectedDiscount := decimal.NewFromFloat(100.00)
	if !totals.Discount.Equal(expectedDiscount) {
		t.Errorf("expected discount %s, got %s", expectedDiscount, totals.Discount)
	}

	// Taxable: 2100.00 * 0.18 = 378.00
	expectedTax := decimal.NewFromFloat(378.00)
	if !totals.TaxAmount.Equal(expectedTax) {
		t.Errorf("expected tax %s, got %s", expectedTax, totals.TaxAmount)
	}

	// Grand Total: 2100.00 + 378.00 = 2478.00
	expectedGrandTotal := decimal.NewFromFloat(2478.00)
	if !totals.GrandTotal.Equal(expectedGrandTotal) {
		t.Errorf("expected grand total %s, got %s", expectedGrandTotal, totals.GrandTotal)
	}
}

func TestCalculateOrderTotals_NegativePrice(t *testing.T) {
	items := []LineItem{
		{
			SKU:       "SKU-BAD",
			Quantity:  1,
			UnitPrice: decimal.NewFromFloat(-50.00),
		},
	}
	_, err := CalculateOrderTotals(items, decimal.NewFromFloat(0.18))
	if err != ErrNegativeAmount {
		t.Errorf("expected ErrNegativeAmount, got %v", err)
	}
}
