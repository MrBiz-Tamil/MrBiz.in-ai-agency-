import csv
import io
import os
import subprocess
import sys
from datetime import datetime, timezone

import weekly_report as wr

HERE = os.path.dirname(os.path.abspath(__file__))
SAMPLE = os.path.join(HERE, "sample_orders.csv")


def load(**kw):
    with open(SAMPLE, newline="", encoding="utf-8-sig") as f:
        return wr.load_orders(csv.DictReader(f), kw.get("source_col", "Source"), kw.get("rto_tag", "RTO"))


def test_extra_line_item_rows_are_not_counted_as_orders():
    orders = load()
    assert len(orders) == 10  # #1003 has a second line-item row that must be skipped
    assert [o["name"] for o in orders].count("#1003") == 1


def test_weekly_totals_and_flags():
    weeks = wr.summarise(load())
    by_start = {s.strftime("%Y-%m-%d"): w for s, w in weeks.items()}
    w1 = by_start["2026-10-26"]  # Mon 26 Oct – Sun 1 Nov
    assert w1["orders"] == 3 and w1["revenue"] == 1499 + 899 + 2398
    assert w1["cod"] == 1 and w1["rto"] == 1
    w2 = by_start["2026-11-02"]
    assert w2["orders"] == 4
    assert w2["cancelled"] == 1 and w2["revenue"] == 1299 + 999 + 2199  # cancelled order excluded
    assert w2["refunded"] == 999
    w3 = by_start["2026-11-09"]
    assert w3["orders"] == 3 and w3["rto"] == 1


def test_repeat_customers_counted_from_first_seen():
    weeks = wr.summarise(load())
    by_start = {s.strftime("%Y-%m-%d"): w for s, w in weeks.items()}
    # week 1: asha, ravi, meera are all new
    assert by_start["2026-10-26"]["new"] == 3 and by_start["2026-10-26"]["repeat"] == 0
    # week 2: asha (repeat), kabir (new), meera (repeat), neha (new)
    assert by_start["2026-11-02"]["repeat"] == 2 and by_start["2026-11-02"]["new"] == 2


def test_rto_tag_can_be_disabled_and_source_column_changed():
    assert sum(o["rto"] for o in load(rto_tag="")) == 0
    assert {o["source"] for o in load(source_col="Nope")} == {"unknown"}


def test_money_and_dates_parse_defensively():
    assert wr.parse_money("₹1,499.00") == 1499.0
    assert wr.parse_money("") == 0.0 and wr.parse_money("abc") == 0.0
    assert wr.parse_date("2026-11-02 14:03:11 +0530").utcoffset().total_seconds() == 19800
    assert wr.parse_date("garbage") is None and wr.parse_date("") is None
    assert wr.week_start(datetime(2026, 11, 8, 23, 0, tzinfo=timezone.utc)).strftime("%Y-%m-%d") == "2026-11-02"


def test_report_renders_expected_sections():
    text = wr.render(wr.summarise(load()), 4)
    assert "# Weekly sales report" in text
    assert "| 26 Oct |" in text and "| 09 Nov |" in text
    assert "Orders by source" in text and "instagram" in text
    assert "Change vs previous week" in text
    assert "asha@example.com" not in text  # customer emails are never printed


def test_empty_file_and_bad_file(tmp_path):
    empty = tmp_path / "e.csv"
    empty.write_text("Name,Email,Created at,Total\n", encoding="utf-8")
    assert wr.main([str(empty)]) == 0
    bad = tmp_path / "b.csv"
    bad.write_text("foo,bar\n1,2\n", encoding="utf-8")
    assert wr.main([str(bad)]) == 2
    assert wr.main([str(tmp_path / "missing.csv")]) == 2


def test_command_line_runs_end_to_end():
    r = subprocess.run([sys.executable, os.path.join(HERE, "weekly_report.py"), SAMPLE, "--weeks", "2"],
                       capture_output=True, text=True, encoding="utf-8")
    assert r.returncode == 0
    assert "| 26 Oct |" not in r.stdout  # only the last 2 weeks are shown
    assert "| 02 Nov |" in r.stdout and "| 09 Nov |" in r.stdout
