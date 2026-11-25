# WhatsApp Message Templates (12)

WhatsApp only lets you start a conversation (or message outside the 24-hour customer window) with a
**pre-approved template**. Submit these to Meta through your inbox tool (or Meta's template manager) in
week 1. Approval is not instant, so submit early.

## Read this first

- **Seven are Utility** (about an order the customer placed) and **five are Marketing** (promotional).
  Meta decides the final category, not you. If you label a promotional message as Utility, Meta can
  reclassify it (and charge accordingly) or reject it. When in doubt, choose Marketing.
- **Costs differ by category** and market. Service messages (replying inside the 24-hour window) are
  free, and so are utility messages sent in reply to a customer (verified on Meta's pricing page).
  Check the current India rate card for the rest before you plan campaigns.
- **Only message customers who opted in.** Capture consent at checkout and keep the record
  (see the opt-in wording at the bottom).
- Variables are written `{{1}}`, `{{2}}`: your tool fills them from the order. Meta asks for
  example values; they are included below.
- Meta has often rejected templates that **start or end with a variable**, and ones that mix
  promotion into a utility message. These templates are structured to avoid that. If one is rejected,
  read Meta's reason, adjust the wording, and resubmit.
- Keep language simple. Submit an English version; add Hindi/Hinglish versions later for customers who
  prefer them.
- Brand name below is `{{brand}}`: replace it with your real brand name **before** submitting
  (this one is plain text, not a variable).

---

## UTILITY templates (order updates)

### 1. `order_confirmation`
- **Category:** Utility · **Language:** English
- **Trigger:** Order is placed and paid (or confirmed)
- **Body:**
  > Hi {{1}}, thank you for your order with {{brand}}! 🎉
  > Order number: {{2}}
  > Total: ₹{{3}}
  > We'll update you as soon as it ships.
- **Footer:** {{brand}}
- **Button (URL):** "View order" → your order status page
- **Example values:** 1 = Priya, 2 = #1042, 3 = 1,499

### 2. `cod_order_confirmation`
- **Category:** Utility · **Language:** English
- **Trigger:** COD order placed
- **Body:**
  > Hi {{1}}, we received your Cash on Delivery order {{2}} for ₹{{3}}.
  > To dispatch it, please confirm that the details are correct.
- **Buttons (quick reply):** "Confirm order" · "Change address" · "Cancel order"
- **Example values:** 1 = Priya, 2 = #1043, 3 = 1,299
- **Notes:** Handle the three replies: Confirm → release to dispatch; Change address → human;
  Cancel → cancel the order. No reply after 24 hours → one reminder, then pause dispatch.

### 3. `order_shipped`
- **Category:** Utility · **Language:** English
- **Trigger:** Courier picks up the parcel
- **Body:**
  > Good news, {{1}}! Your order {{2}} has shipped with {{3}}.
  > Tracking number: {{4}}
  > Delivery usually takes a few working days.
- **Button (URL):** "Track order" → courier tracking link
- **Example values:** 1 = Priya, 2 = #1042, 3 = Delhivery, 4 = 1234567890

### 4. `out_for_delivery`
- **Category:** Utility · **Language:** English
- **Trigger:** Courier marks "out for delivery"
- **Body:**
  > Hi {{1}}, your order {{2}} is out for delivery today.
  > Please keep your phone nearby so the delivery partner can reach you.
- **Example values:** 1 = Priya, 2 = #1042

### 5. `delivery_confirmation`
- **Category:** Utility · **Language:** English
- **Trigger:** Courier marks "delivered"
- **Body:**
  > Hi {{1}}, your order {{2}} has been delivered.
  > If anything isn't right, reply here within {{3}} days and we'll help with an exchange or return.
- **Example values:** 1 = Priya, 2 = #1042, 3 = 7  (use your real `{{return_window}}`)

### 6. `return_request_received`
- **Category:** Utility · **Language:** English
- **Trigger:** Customer requests a return or exchange
- **Body:**
  > Hi {{1}}, we've received your request for order {{2}}.
  > Reference: {{3}}
  > Next step: {{4}}
- **Example values:** 1 = Priya, 2 = #1042, 3 = RET-2210, 4 = Our courier will contact you for pickup

### 7. `refund_initiated`
- **Category:** Utility · **Language:** English
- **Trigger:** Refund is processed
- **Body:**
  > Hi {{1}}, your refund of ₹{{2}} for order {{3}} has been started.
  > It will reach your {{4}} in about {{5}}.
- **Example values:** 1 = Priya, 2 = 1,499, 3 = #1042, 4 = original payment method, 5 = 5–7 working days

---

## MARKETING templates (need clear opt-in; include an opt-out)

### 8. `abandoned_cart_reminder`
- **Category:** Marketing · **Language:** English
- **Trigger:** Cart left for about 3 hours (send **once**, never a drip of reminders)
- **Body:**
  > Hi {{1}}, you left something lovely in your bag. 🛍️
  > {{2}} is still waiting for you.
  > Complete your order whenever you're ready.
- **Footer:** Reply STOP to opt out
- **Button (URL):** "Back to my bag" → cart recovery link
- **Example values:** 1 = Priya, 2 = Linen Co-ord Set

### 9. `back_in_stock_alert`
- **Category:** Marketing · **Language:** English
- **Trigger:** A size the customer asked to be notified about is restocked
- **Body:**
  > Good news, {{1}}! The {{2}} in size {{3}} is back in stock.
  > Popular sizes sell out fast, so grab yours while it lasts.
- **Footer:** Reply STOP to opt out
- **Button (URL):** "Shop now" → product page
- **Example values:** 1 = Priya, 2 = Cotton Kurta, 3 = M
- **Notes:** Only send to people who replied YES to being notified. Even then, Meta may treat it as
  Marketing, so submit it as Marketing.

### 10. `new_collection_launch`
- **Category:** Marketing · **Language:** English
- **Trigger:** New drop (a few times a month at most)
- **Body:**
  > Hi {{1}}, our new {{2}} collection just dropped. ✨
  > Take the first look before it goes live to everyone.
- **Footer:** Reply STOP to opt out
- **Button (URL):** "See the collection" → collection page
- **Example values:** 1 = Priya, 2 = Festive Edit

### 11. `reorder_winback`
- **Category:** Marketing · **Language:** English
- **Trigger:** 45–60 days after a customer's last order
- **Body:**
  > Hi {{1}}, it's been a while! We'd love to see you again.
  > Use code {{2}} on your next order for {{3}} off.
  > Valid till {{4}}.
- **Footer:** Reply STOP to opt out
- **Button (URL):** "Shop now" → homepage
- **Example values:** 1 = Priya, 2 = WELCOME10, 3 = 10%, 4 = 30 Nov
- **Notes:** Only use a code you have actually created in the store.

### 12. `review_request`
- **Category:** Marketing · **Language:** English
- **Trigger:** 5–7 days after confirmed delivery, **only if** there was no complaint or return
- **Body:**
  > Hi {{1}}, we hope you're enjoying your {{2}}! 💛
  > Would you share a quick review with a photo? It really helps other shoppers.
- **Footer:** Reply STOP to opt out
- **Button (URL):** "Write a review" → review page
- **Example values:** 1 = Priya, 2 = Cotton Kurta

---

## Opt-in wording (put this at checkout and on your forms)

> ☐ Send me order updates on WhatsApp.
> ☐ Also send me offers and new arrivals on WhatsApp. (You can opt out anytime by replying STOP.)

Keep the two ticks separate: the first covers utility updates, the second covers marketing. Store
the time, number, and what they agreed to.

## Sending rules that protect your number

1. **Frequency:** at most 1–2 marketing messages a week to anyone; one abandoned-cart nudge per cart.
2. **Quality:** if many customers block or report you, Meta lowers your messaging limits. Fewer,
   better messages beat blasts.
3. **STOP means stop**, immediately and permanently. Tag the customer `opted-out` and exclude them from
   every campaign.
4. **Never send** a marketing template to a number that has not opted in.
5. **Test** every template on your own phone before you turn automation on.
