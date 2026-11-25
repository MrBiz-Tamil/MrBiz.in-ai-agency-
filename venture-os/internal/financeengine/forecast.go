package financeengine

import (
	"time"
)

// Direction of a cash movement.
type Direction string

const (
	Inflow  Direction = "INFLOW"
	Outflow Direction = "OUTFLOW"
)

// Item is a scheduled cash movement (a row of cashflow_items).
type Item struct {
	Direction   Direction
	AmountMinor int64
	Due         time.Time
	Status      string // FORECAST | COMMITTED | SETTLED | CANCELLED
	Category    string
}

// Options tune the forecast.
type Options struct {
	// IncludeForecastInflows also counts FORECAST (not yet committed) inflows. Off by default: money we merely hope
	// for is not allowed to extend the runway.
	IncludeForecastInflows bool
	// BucketDays is the width of reporting buckets (default 7).
	BucketDays int
}

// Bucket is one reporting period.
type Bucket struct {
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"` // inclusive
	InflowMinor  int64     `json:"inflow_minor"`
	OutflowMinor int64     `json:"outflow_minor"`
	NetMinor     int64     `json:"net_minor"`
	ClosingMinor int64     `json:"closing_minor"`
}

// Forecast is the projected cash position.
type Forecast struct {
	OpeningMinor    int64     `json:"opening_minor"`
	From            time.Time `json:"from"`
	Days            int       `json:"days"`
	Buckets         []Bucket  `json:"buckets"`
	MinClosingMinor int64     `json:"min_closing_minor"`
	MinClosingDate  time.Time `json:"min_closing_date"`
	EndingMinor     int64     `json:"ending_minor"`
	// RunwayDays is the number of days from From until the balance first goes negative; nil if it stays
	// non-negative for the whole horizon.
	RunwayDays          *int  `json:"runway_days,omitempty"`
	LowCash             bool  `json:"low_cash"`
	OverdueOutflowMinor int64 `json:"overdue_outflow_minor"` // counted on day 0
	OverdueInflowMinor  int64 `json:"overdue_inflow_minor"`  // NOT counted: reported so someone chases it
}

// MaxForecastDays bounds the horizon.
const MaxForecastDays = 366

func day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ForecastCash projects the balance day by day for `days` days starting at `from`.
//
// Settled and cancelled items are ignored (settled money is already in the opening balance). Items due before
// `from` are overdue: overdue outflows are assumed to be paid on day 0 (the cautious assumption); overdue inflows
// are excluded and reported separately.
func ForecastCash(opening int64, items []Item, from time.Time, days int, opt Options) (Forecast, error) {
	if days < 1 || days > MaxForecastDays {
		return Forecast{}, invalid("days must be between 1 and %d", MaxForecastDays)
	}
	bucketDays := opt.BucketDays
	if bucketDays == 0 {
		bucketDays = 7
	}
	if bucketDays < 1 || bucketDays > days {
		return Forecast{}, invalid("bucket_days must be between 1 and the horizon")
	}
	from = day(from)
	if opening < -MaxAmountMinor || opening > MaxAmountMinor {
		return Forecast{}, invalid("opening balance is out of range")
	}

	in := make([]int64, days)
	out := make([]int64, days)
	f := Forecast{OpeningMinor: opening, From: from, Days: days}
	for i, it := range items {
		if it.Status == "SETTLED" || it.Status == "CANCELLED" {
			continue
		}
		if it.Direction != Inflow && it.Direction != Outflow {
			return Forecast{}, invalid("item %d has unknown direction %q", i, it.Direction)
		}
		if err := checkAmount("item amount", it.AmountMinor); err != nil {
			return Forecast{}, err
		}
		if it.Direction == Inflow && it.Status != "COMMITTED" && !opt.IncludeForecastInflows {
			continue
		}
		offset := int(day(it.Due).Sub(from).Hours() / 24)
		if offset < 0 {
			if it.Direction == Outflow {
				f.OverdueOutflowMinor += it.AmountMinor
				offset = 0
			} else {
				f.OverdueInflowMinor += it.AmountMinor
				continue
			}
		}
		if offset >= days {
			continue
		}
		if it.Direction == Inflow {
			in[offset] += it.AmountMinor
		} else {
			out[offset] += it.AmountMinor
		}
	}

	// A day's inflows and outflows settle together at end of day; the intraday order is unknowable.
	bal := opening
	f.MinClosingMinor, f.MinClosingDate = bal, from
	for d := 0; d < days; d++ {
		date := from.AddDate(0, 0, d)
		if d%bucketDays == 0 {
			f.Buckets = append(f.Buckets, Bucket{Start: date})
		}
		cur := &f.Buckets[len(f.Buckets)-1]
		bal += in[d] - out[d]
		cur.InflowMinor += in[d]
		cur.OutflowMinor += out[d]
		cur.NetMinor += in[d] - out[d]
		cur.ClosingMinor = bal
		cur.End = date
		if bal < f.MinClosingMinor {
			f.MinClosingMinor, f.MinClosingDate = bal, date
		}
		if bal < 0 && f.RunwayDays == nil {
			r := d + 1
			f.RunwayDays = &r
		}
	}
	f.EndingMinor = bal
	f.LowCash = f.MinClosingMinor < 0
	return f, nil
}
