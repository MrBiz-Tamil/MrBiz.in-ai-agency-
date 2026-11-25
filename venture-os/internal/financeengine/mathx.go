// Package financeengine is the deterministic arithmetic of Venture OS: pricing for SaaS tiers, D2C unit economics
// and export/import landed cost; invoice and purchase-order totals; and cash-flow forecasting.
//
// It is the only place agents' money numbers come from. Rules:
//   - Money is int64 minor units, rates are integer basis points; there is no floating point anywhere.
//   - Intermediate products use math/big, so nothing overflows silently; results that do not fit are errors.
//   - Rounding is explicit: costs round half up; prices derived from a target margin round UP, so a derived price
//     can never fall below the margin it was derived for.
//   - Functions are pure: the same inputs always give the same outputs, and they do no I/O.
package financeengine

import (
	"errors"
	"fmt"
	"math/big"
)

// MaxAmountMinor bounds every monetary input (about 10^15 minor units) so sums of many items stay far below int64.
const MaxAmountMinor int64 = 1_000_000_000_000_000

// ErrInvalid wraps every input-validation failure, so callers can tell a bad request from a bug.
var ErrInvalid = errors.New("invalid input")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func checkAmount(name string, v int64) error {
	if v < 0 {
		return invalid("%s must not be negative", name)
	}
	if v > MaxAmountMinor {
		return invalid("%s is too large", name)
	}
	return nil
}

func checkBps(name string, v int) error {
	if v < 0 || v > 10000 {
		return invalid("%s must be between 0 and 10000 basis points", name)
	}
	return nil
}

func toInt64(b *big.Int) (int64, error) {
	if !b.IsInt64() {
		return 0, invalid("result does not fit in 64 bits")
	}
	return b.Int64(), nil
}

// mulDivRound returns round_half_up(a*b/c) for non-negative a, b and positive c.
func mulDivRound(a, b, c int64) (int64, error) {
	if a < 0 || b < 0 || c <= 0 {
		return 0, invalid("mulDivRound(%d,%d,%d)", a, b, c)
	}
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	n.Add(n, big.NewInt(c/2))
	n.Quo(n, big.NewInt(c))
	return toInt64(n)
}

// mulDivCeil returns ceil(a*b/c) for non-negative a, b and positive c.
func mulDivCeil(a, b, c int64) (int64, error) {
	if a < 0 || b < 0 || c <= 0 {
		return 0, invalid("mulDivCeil(%d,%d,%d)", a, b, c)
	}
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	n.Add(n, big.NewInt(c-1))
	n.Quo(n, big.NewInt(c))
	return toInt64(n)
}

// bps returns round_half_up(amount * rate / 10000).
func bps(amount int64, rate int) (int64, error) { return mulDivRound(amount, int64(rate), 10000) }

func mul(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, invalid("negative factor")
	}
	return toInt64(new(big.Int).Mul(big.NewInt(a), big.NewInt(b)))
}

func add(vals ...int64) (int64, error) {
	sum := new(big.Int)
	for _, v := range vals {
		sum.Add(sum, big.NewInt(v))
	}
	return toInt64(sum)
}

// marginBps returns floor((price-cost)*10000/price), which is negative when the sale makes a loss. Rounding
// down means a displayed margin is never better than the real one.
func marginBps(price, cost int64) int {
	if price <= 0 {
		return -10000
	}
	n := new(big.Int).Mul(big.NewInt(price-cost), big.NewInt(10000))
	q, m := new(big.Int).DivMod(n, big.NewInt(price), new(big.Int))
	_ = m // DivMod is Euclidean: for a positive divisor this is floor division
	if !q.IsInt64() {
		return -10000
	}
	return int(q.Int64())
}

// belowFloor reports, exactly and without rounding, whether (price-cost)/price < floorBps/10000.
func belowFloor(price, cost int64, floorBps int) bool {
	if price <= 0 {
		return true
	}
	lhs := new(big.Int).Mul(big.NewInt(price-cost), big.NewInt(10000))
	rhs := new(big.Int).Mul(big.NewInt(price), big.NewInt(int64(floorBps)))
	return lhs.Cmp(rhs) < 0
}

// MinorDigits is the number of minor-unit digits of an ISO-4217 currency (2 for most, 0 for JPY-like, 3 for KWD-like).
func MinorDigits(currency string) int {
	switch currency {
	case "JPY", "KRW", "VND", "CLP", "ISK", "UGX", "PYG", "XAF", "XOF", "XPF", "RWF", "KMF", "GNF", "DJF", "VUV":
		return 0
	case "KWD", "BHD", "OMR", "JOD", "TND", "IQD", "LYD":
		return 3
	}
	return 2
}

// FormatMinor renders minor units as text for human-readable summaries, exactly (no floats): FormatMinor(1180562,
// "INR") == "INR 11,805.62". Thousands are grouped in threes.
func FormatMinor(amount int64, currency string) string {
	digits := MinorDigits(currency)
	neg := amount < 0
	abs := new(big.Int).Abs(big.NewInt(amount)).String()
	for len(abs) < digits+1 {
		abs = "0" + abs
	}
	whole, frac := abs[:len(abs)-digits], abs[len(abs)-digits:]
	var grouped []byte
	for i, c := range []byte(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, c)
	}
	out := string(grouped)
	if digits > 0 {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return currency + " " + out
}
