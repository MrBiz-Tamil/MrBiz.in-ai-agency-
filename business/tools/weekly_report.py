#!/usr/bin/env python3
"""Weekly sales report from a Shopify orders CSV export. Standard library only.

Usage:
    python weekly_report.py orders_export.csv                # last 4 weeks, Markdown to the screen
    python weekly_report.py orders_export.csv --weeks 8 --out report.md
    python weekly_report.py orders_export.csv --rto-tag RTO --source-col Source

The numbers (per ISO week, Monday start):
    orders, revenue, average order value, COD share, cancelled, refunded amount,
    RTO count (orders carrying your RTO tag), new vs repeat customers, orders by source.

Notes on the export:
  * Shopify writes one row per line item. Only the first row of an order has the order total, so rows
    without a "Total" are treated as extra items of the previous order and skipped.
  * Repeat customers are counted from the orders in the file. Export a long date range (e.g. 6+ months)
    for a meaningful repeat rate.
  * "Return to origin" is not a Shopify field. Tag those orders in Shopify (e.g. "RTO") and pass --rto-tag.
  * The customer email is used only to recognise repeat buyers and is never printed.
"""
import argparse
import csv
import sys
from collections import Counter, defaultdict
from datetime import datetime, timedelta, timezone
from typing import Dict, Iterable, List, Optional

DATE_FORMATS = (
    "%Y-%m-%d %H:%M:%S %z",  # Shopify: 2026-11-02 14:03:11 +0530
    "%Y-%m-%d %H:%M:%S",
    "%Y-%m-%dT%H:%M:%S%z",
    "%Y-%m-%d",
)


def parse_date(raw: str) -> Optional[datetime]:
    raw = (raw or "").strip()
    if not raw:
        return None
    for fmt in DATE_FORMATS:
        try:
            d = datetime.strptime(raw, fmt)
            return d if d.tzinfo else d.replace(tzinfo=timezone.utc)
        except ValueError:
            continue
    return None


def parse_money(raw: str) -> float:
    raw = (raw or "").replace(",", "").replace("₹", "").replace("Rs.", "").strip()
    try:
        return float(raw)
    except ValueError:
        return 0.0


def week_start(d: datetime) -> datetime:
    d = d.astimezone(timezone.utc)
    return (d - timedelta(days=d.weekday())).replace(hour=0, minute=0, second=0, microsecond=0)


def load_orders(rows: Iterable[Dict[str, str]], source_col: str, rto_tag: str) -> List[dict]:
    """One dict per order (deduplicated from line-item rows)."""
    orders: List[dict] = []
    for row in rows:
        total_raw = (row.get("Total") or "").strip()
        created = parse_date(row.get("Created at", ""))
        if not total_raw or created is None:
            continue  # extra line-item row, or an unreadable row
        tags = [t.strip().lower() for t in (row.get("Tags") or "").split(",") if t.strip()]
        payment = (row.get("Payment Method") or "").lower()
        orders.append({
            "name": (row.get("Name") or "").strip(),
            "email": (row.get("Email") or "").strip().lower(),
            "created": created,
            "total": parse_money(total_raw),
            "cod": "cod" in payment or "cash on delivery" in payment,
            "cancelled": bool((row.get("Cancelled at") or "").strip()),
            "refunded": parse_money(row.get("Refunded Amount", "")),
            "rto": bool(rto_tag) and rto_tag.lower() in tags,
            "source": (row.get(source_col) or "unknown").strip() or "unknown",
        })
    return sorted(orders, key=lambda o: o["created"])


def summarise(orders: List[dict]) -> Dict[datetime, dict]:
    """Per-week metrics. A customer is 'repeat' if they ordered earlier in the file."""
    seen_customers = set()
    weeks: Dict[datetime, dict] = defaultdict(lambda: {
        "orders": 0, "revenue": 0.0, "cod": 0, "cancelled": 0, "refunded": 0.0,
        "rto": 0, "new": 0, "repeat": 0, "sources": Counter(),
    })
    for o in orders:
        w = weeks[week_start(o["created"])]
        w["orders"] += 1
        if not o["cancelled"]:
            w["revenue"] += o["total"]
        w["cod"] += o["cod"]
        w["cancelled"] += o["cancelled"]
        w["refunded"] += o["refunded"]
        w["rto"] += o["rto"]
        w["sources"][o["source"]] += 1
        if o["email"]:
            if o["email"] in seen_customers:
                w["repeat"] += 1
            else:
                w["new"] += 1
                seen_customers.add(o["email"])
    return weeks


def pct(part: float, whole: float) -> str:
    return f"{part / whole * 100:.1f}%" if whole else "n/a"


def render(weeks: Dict[datetime, dict], n_weeks: int) -> str:
    if not weeks:
        return "No orders found in the file.\n"
    starts = sorted(weeks)[-n_weeks:]
    lines = [
        f"# Weekly sales report ({starts[0]:%d %b %Y} – {(starts[-1] + timedelta(days=6)):%d %b %Y})",
        "",
        "| Week of | Orders | Revenue | AOV | COD share | Cancelled | Refunded | RTO | Repeat share |",
        "|---|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for s in starts:
        w = weeks[s]
        live_orders = w["orders"] - w["cancelled"]
        aov = f"₹{w['revenue'] / live_orders:,.0f}" if live_orders else "n/a"
        lines.append(
            f"| {s:%d %b} | {w['orders']} | ₹{w['revenue']:,.0f} | {aov} | {pct(w['cod'], w['orders'])} | "
            f"{w['cancelled']} | ₹{w['refunded']:,.0f} | {w['rto']} | {pct(w['repeat'], w['repeat'] + w['new'])} |"
        )

    latest = weeks[starts[-1]]
    lines += ["", f"## Orders by source, week of {starts[-1]:%d %b}", ""]
    if latest["sources"]:
        lines += ["| Source | Orders | Share |", "|---|---:|---:|"]
        for src, n in latest["sources"].most_common():
            lines.append(f"| {src} | {n} | {pct(n, latest['orders'])} |")
    if len(starts) >= 2:
        prev = weeks[starts[-2]]
        lines += ["", "## Change vs previous week", ""]
        for label, a, b in (("Orders", latest["orders"], prev["orders"]), ("Revenue", latest["revenue"], prev["revenue"])):
            change = "n/a" if not b else f"{(a - b) / b * 100:+.1f}%"
            lines.append(f"- {label}: {change}")
    lines += [
        "",
        "_Reading tips: a rising COD share with rising RTO means confirm COD orders before dispatch. "
        "A falling repeat share means your review/reorder messages need attention._",
    ]
    return "\n".join(lines) + "\n"


def main(argv: Optional[List[str]] = None) -> int:
    ap = argparse.ArgumentParser(description="Weekly sales report from a Shopify orders CSV export")
    ap.add_argument("csv_file")
    ap.add_argument("--weeks", type=int, default=4, help="how many recent weeks to show (default 4)")
    ap.add_argument("--rto-tag", default="RTO", help='order tag marking returns-to-origin (default "RTO"; "" to disable)')
    ap.add_argument("--source-col", default="Source", help='column holding the order source (default "Source")')
    ap.add_argument("--out", help="write the report to this file instead of the screen")
    args = ap.parse_args(argv)
    if args.weeks < 1:
        ap.error("--weeks must be at least 1")

    try:
        with open(args.csv_file, newline="", encoding="utf-8-sig") as f:
            reader = csv.DictReader(f)
            if not reader.fieldnames or "Created at" not in reader.fieldnames or "Total" not in reader.fieldnames:
                print('This does not look like a Shopify orders export (needs "Created at" and "Total" columns).', file=sys.stderr)
                return 2
            orders = load_orders(reader, args.source_col, args.rto_tag)
    except OSError as e:
        print(f"Cannot read file: {e}", file=sys.stderr)
        return 2

    report = render(summarise(orders), args.weeks)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(report)
        print(f"Report written to {args.out}")
    else:
        # Windows consoles often use a legacy code page that cannot show the rupee sign.
        try:
            sys.stdout.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, ValueError):
            pass
        print(report)
    return 0


if __name__ == "__main__":
    sys.exit(main())
