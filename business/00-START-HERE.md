# Clothing Store Playbook: Start Here

Everything in this folder is written for an online clothing store handling about 50 chats a day
across WhatsApp, Instagram, Facebook and your website. It is ready to copy into your inbox tool.

## How to use these files

| File | What it is | When you use it |
|---|---|---|
| `00-START-HERE.md` | This guide and the **Brand Facts Sheet** | Fill it in first (30 minutes) |
| `01-faq-answers.md` | 36 ready answers to the questions customers ask most | Load into your inbox as saved replies / AI knowledge |
| `02-whatsapp-templates.md` | 12 WhatsApp message templates for Meta approval | Submit in week 1; approval takes time |
| `03-instagram-flows.md` | Comment-to-DM, story reply and ad-click flows | Set up in week 2 |
| `04-chat-scripts.md` | Scripts for hard situations + rules for what the AI may and may not say | Train anyone who answers chats |
| `05-sales-process.md` | Pipeline, tags, daily and weekly routine, numbers to watch | Run your week from this |
| `tools/weekly_report.py` | Turns your Shopify orders export into a weekly numbers report | Every Monday |

Anything written like `{{return_window}}` is a **blank to fill in from the Brand Facts Sheet below**.
A few blanks (`{{name}}`, `{{product_link}}`, `{{item}}`, `{{size}}`, `{{sizes}}`, `{{product_name}}`) are filled
automatically by your inbox tool for each customer; those you do not fill in yourself.
Nothing here is a promise until you fill it in. Never publish a template that still contains `{{...}}`
placeholders you have not replaced (the numbered `{{1}}`, `{{2}}` in WhatsApp templates are different:
those are variables that your tool fills in automatically).

---

## Brand Facts Sheet (fill this in once)

Copy this table somewhere you can edit it. Every answer in the FAQ draws from it. Keep it short and true.

| Placeholder | Your answer |
|---|---|
| `{{brand}}` | Your brand name |
| `{{support_hours}}` | e.g. Mon–Sat, 10 am – 7 pm |
| `{{reply_time}}` | e.g. within 15 minutes in working hours |
| `{{website}}` | Your website address |
| `{{size_chart_link}}` | Link to your size chart page |
| `{{delivery_days_metro}}` | e.g. 2–4 working days |
| `{{delivery_days_other}}` | e.g. 4–7 working days |
| `{{dispatch_time}}` | e.g. within 24 hours of order |
| `{{shipping_fee}}` | e.g. free above ₹999, otherwise ₹79 |
| `{{cod_available}}` | Yes / No / only for orders under ₹X |
| `{{cod_fee}}` | e.g. ₹50, or "no extra fee" |
| `{{payment_modes}}` | e.g. UPI, cards, net banking, wallets, COD |
| `{{return_window}}` | e.g. 7 days from delivery |
| `{{return_conditions}}` | e.g. unworn, tags attached, original packaging |
| `{{return_pickup}}` | e.g. we arrange pickup; free for exchanges |
| `{{refund_time}}` | e.g. 5–7 working days after we receive the item |
| `{{refund_method}}` | e.g. original payment method; COD orders to bank/UPI |
| `{{exchange_policy}}` | e.g. one free size exchange per order |
| `{{non_returnable}}` | e.g. innerwear, sale items, customised items |
| `{{discount_policy}}` | e.g. we don't negotiate prices; codes are on our page |
| `{{max_discount_ai}}` | The most the AI may ever offer. **Set to 0% unless you decide otherwise** |
| `{{bulk_min}}` | e.g. 10 pieces or more |
| `{{fabric_care}}` | e.g. machine wash cold, do not bleach |
| `{{fit_note}}` | e.g. our sizing runs true / slightly relaxed fit |
| `{{restock_policy}}` | e.g. best sellers restock every 3–4 weeks |
| `{{human_escalation_contact}}` | Who takes over when the AI hands off (name or number) |
| `{{replacement_or_refund_option}}` | What you offer for wrong/damaged items, e.g. a free replacement or a full refund |
| `{{review_reward}}` | e.g. 10% off the next order (only if you really offer it) |
| `{{whatsapp_link}}` | `https://wa.me/<number with country code, no + or spaces>` |
| `{{collection_link}}` | Link to your main collection page |

### Decisions only you can make (and the safe default)

| Decision | Safe default |
|---|---|
| Can the AI give discounts? | **No.** It may only share codes already public on your website |
| Can the AI promise refunds? | **No.** It collects details and hands over to a person |
| Can the AI promise a delivery date? | Only quote the standard range, never an exact day |
| Languages | Start with English and Hinglish; add others when customers ask |
| Hours | Auto-reply outside hours: "We'll reply at {{support_hours}}" |

---

## Before you start: five things only you can do

1. **Verify your business on Meta** (Business Manager). This unlocks the WhatsApp Business Platform.
2. **Connect WhatsApp, Instagram and your Facebook Page** to one inbox tool through its official sign-in flow.
3. **Get your templates approved** (file `02`). Submit them early because approval is not instant.
4. **Turn on customer consent capture** at checkout (a tick box: "Send me order updates on WhatsApp") and keep the record.
5. **Check the current rules.** Meta's template categories and pricing change. Read Meta's current
   category guidelines before submitting, and confirm the India rate card for marketing messages.

I can't do these for you: they need your logins and your business documents.
