# Weekly report tool

Turns a Shopify orders export into the numbers you check every Monday. Needs Python 3.8+ (nothing to install).

## 1. Export your orders (2 minutes)
Shopify admin → **Orders** → **Export** → choose a long date range (at least 6 months, so repeat
customers can be recognised) → "Plain CSV file".

## 2. Tag returns-to-origin
Shopify doesn't track orders refused or undelivered. When an order comes back, add the tag **RTO** to it.
(Use a different tag if you like and pass `--rto-tag YourTag`.)

## 3. Run it
```powershell
python weekly_report.py orders_export.csv                    # last 4 weeks
python weekly_report.py orders_export.csv --weeks 8 --out report.md
```

You get a table per week: orders, revenue, average order value, COD share, cancellations, refunds,
RTO count and repeat-customer share, plus orders by source and the change vs the previous week.

## Notes
- **Source:** the report groups by the export's `Source` column. In Shopify that is usually the sales
  channel (web, POS), not Instagram vs Facebook. For channel-level reporting, tag orders (for example
  `instagram`) in Shopify and run `--source-col Tags`, or add a column named `Source` to the CSV.
- **Customer emails** are used only to spot repeat buyers and are never printed.
- It reads the export file only. It never connects to your store or sends anything anywhere.
- A sample file is included: `python weekly_report.py sample_orders.csv`.
- Tests: `python -m pytest test_weekly_report.py`.
