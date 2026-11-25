# Sales Process and Weekly Routine

## 1. The pipeline (one board in your inbox/CRM)

| Stage | Meaning | Action | Move on when |
|---|---|---|---|
| **New** | First message received | Instant auto-reply, tag the source | A person or the AI has answered |
| **Interested** | They asked about an item, size or price | Send the product link, check stock | They say they want to buy |
| **Ready to buy** | Size and item agreed | Send checkout link / payment | Order is placed |
| **Ordered** | Order placed (COD: awaiting confirmation) | Confirm details | Dispatched |
| **Delivered** | Courier marked delivered | Delivery message, check for issues | 5–7 days pass with no complaint |
| **Repeat** | Ordered again | Thank them, tag `repeat` | : |
| **Lost** | No reply after follow-ups | Tag the reason (price, size, delivery, other) | : |
| **Bulk** (separate board) | 10+ pieces / wholesale / reseller | A person quotes | Won or lost |

**Follow-up rule (automatic):** *Interested* with no reply → one gentle nudge after 24 hours → one
final nudge after 3 days → move to *Lost* with a reason. Never more than two nudges.

## 2. Tags to use (keep it simple)

- **Source:** see `03-instagram-flows.md` (`src-...`).
- **Intent:** `size-help`, `stock-check`, `order-status`, `return`, `exchange`, `complaint`, `bulk`.
- **Status:** `cod-unconfirmed`, `vip` (3+ orders), `repeat`, `opted-out`, `escalated`.

Fewer than 20 tags total. Tags nobody uses are clutter.

## 3. First-response targets

| Situation | Target |
|---|---|
| Any new message, in hours | under 5 minutes (the auto-reply counts as the first touch, a person within 15) |
| Message outside hours | auto-reply instantly, a person at the next opening |
| Complaint or wrong item | a person within 1 hour in working hours |
| Quote for bulk order | same working day |

## 4. Daily routine (one person, 50 chats a day)

**Morning (30 min)**
1. Clear overnight chats, oldest first.
2. Check COD orders tagged `cod-unconfirmed` and send the reminder.
3. Check *Ready to buy* leads that haven't paid, and nudge once.

**Midday and afternoon**
4. Answer chats in batches: don't leave a chat open for hours. If you can't answer, say when you will.
5. Hand any new return/exchange to the right process the same day.

**Evening (20 min)**
6. Tag every chat that ended (won / lost + reason).
7. Add any new question you couldn't answer to `01-faq-answers.md`.
8. Note any Level 2 case and what happened.

## 5. Weekly routine (Monday, 45 minutes)

1. Export your orders from your store and run `tools/weekly_report.py` (instructions in
   `tools/README.md`). It gives you the numbers below.
2. Read the **top 10 questions** from last week. Turn the top 3 into new automatic answers.
3. Look at **returns and complaints**: is one item or size causing most of them? Fix the product page
   (size chart, photos, description) before anything else.
4. Pick **one improvement** for the week. Not five. For example: "cut first-reply time", "send COD
   confirmations", "add size help to the top 5 products".

## 6. Numbers to watch

| Number | How to read it | A sign of trouble |
|---|---|---|
| Chats per day | Demand | Sudden drop after an ad change |
| First-reply time | Speed | Over 15 minutes in working hours |
| Chat → order rate | Sales skill + fit of traffic | Falling, or very different by source |
| Orders by source | Which channel pays | A channel with lots of chats but no orders |
| Average order value | Basket size | Dropping (consider bundles, free-shipping threshold) |
| COD share | Risk | COD share high with high returns |
| Return-to-origin (RTO) rate | Profit leak, COD orders refused or undelivered | Rising (confirm COD orders before dispatch) |
| Repeat-purchase rate | Loyalty | Under about a fifth of customers coming back within 90 days |

(The "about a fifth" figure is a rule of thumb, not a standard. Use your own history as the baseline.)

## 7. How to grow without chaos (in this order)

1. **Fix replies first.** Fast, clear, consistent answers raise conversion for free.
2. **Cut COD returns.** Confirm COD orders on WhatsApp before dispatch.
3. **Use the 72-hour free window.** Run click-to-WhatsApp ads and reply fast. See `03`.
4. **Build the repeat engine.** Review requests, reorder reminders and restock alerts, sent only to
   customers who opted in.
5. **Only then add more tools or more channels.**

## 8. When to upgrade the setup

- You regularly exceed about 100 chats a day, or have 3+ people sharing the inbox → move to a
  stronger team inbox with assignment rules and reports.
- You start selling wholesale or to resellers → add a CRM for the *Bulk* pipeline.
- You want approvals, spend limits, and audit trails around refunds and discounts → that is where your
  ABOS project fits, once the basics above are running.
