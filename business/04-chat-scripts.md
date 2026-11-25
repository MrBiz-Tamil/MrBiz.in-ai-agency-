# Chat Scripts and AI Guardrails

For the moments that decide whether a customer stays or leaves. Use these as starting points;
adapt the wording to sound like you. Replace `{{placeholders}}` from the Brand Facts Sheet.

## Part 1: What the AI may and may not do

### The AI MAY
- Answer product, size, delivery and policy questions **using only your Brand Facts Sheet and live store data**.
- Look up order status and share status + tracking link.
- Share product links and public discount codes.
- Collect details (order number, photos) and hand over.
- Offer to add a customer to a restock/launch list (only with a clear YES).

### The AI MAY NOT (hard rules)
1. **Never promise a refund, replacement, discount, free shipping, or exact delivery date.** Only a person can.
2. **Never go above `{{max_discount_ai}}`** (default 0%).
3. **Never ask for or accept** card numbers, CVV, OTPs, passwords, or UPI PINs. If a customer sends them, say: "Please don't share that, it isn't needed. Please delete the message." Then escalate.
4. **Never guess.** If stock, price, or policy isn't in the data: "Let me check with the team and get back to you."
5. **Never argue** with an upset customer or defend the company. Apologise, collect details, hand over.
6. **Never share** another customer's information, or a full address in chat; confirm only the last part (pincode / area) if needed.
7. **Never give medical, legal, or skin-allergy advice.** Say you'll pass it to the team.

### Hand over to a person IMMEDIATELY when
- The customer is angry, uses legal words ("consumer court", "fraud", "police"), or threatens a bad review.
- Anything involves a refund, wrong/damaged item, or missing parcel.
- The customer asks twice for a person, or the AI has failed to help after two tries.
- A bulk or wholesale request, or a collaboration/influencer request.
- Payment taken but no order confirmation.
- Anything the AI isn't sure about.

Handover message (AI to customer):
> I'm bringing in a teammate who can sort this out properly. 🙏 They'll reply {{reply_time}} (we're
> available {{support_hours}}). I've shared everything you told me, so you won't need to repeat it.

Handover note (AI to team), always include: customer name, phone, order number, what happened,
what the customer wants, photos received, mood (calm / upset / very upset).

---

## Part 2: Scripts for people

### 1. Wrong or damaged item
> I'm really sorry, that's not the experience we want you to have. 🙏
> Could you send a clear photo of the item and the parcel label (with order number)?
> Once I have that, I'll arrange {{replacement_or_refund_option}} for you. Thank you for your patience.

Steps: apologise → get photo + order number → check the order → decide the remedy → tell the customer
exactly what happens next and when → follow up on the day you promised.

### 2. Parcel late / not moving
> I'm sorry for the delay. I understand how frustrating it is to wait. I'm checking with our courier
> right now and will update you by {{reply_time}}.

Then: contact the courier, send the real update (even if the update is "still checking"), and set a
reminder to follow up.

### 3. Customer is angry
> I hear you, and I'm sorry. You're right to expect better. Let me take this over personally.
> Can you tell me what happened, and what would put this right for you?

Rules: do not defend, do not explain at length, ask what they want, then do the achievable part fast.

### 4. Wants a refund (within the window)
> Sure, I'll start that now. Please confirm your order number and the reason, and send a photo if the
> item is damaged. Your refund goes to {{refund_method}} within {{refund_time}} after we receive and
> check the item. {{return_pickup}}

### 5. Wants a refund (outside the window)
> Thanks for letting me know. Our standard window is {{return_window}}, so let me check with the team
> what we can do and come back to you by {{reply_time}}.

The owner decides case by case (loyal customer? genuine fault?). Never decide on the spot to be nice
and then regret it, and never refuse without checking.

### 6. COD customer refuses delivery / doesn't answer confirmation
> Hi {{name}}, we're getting your order ready! 🎉 Please reply **YES** to confirm your COD order and
> address. If we don't hear from you, we may have to cancel it to keep prices fair for everyone.

If there's no reply after 24 hours and one reminder: pause dispatch and tag `cod-unconfirmed`.

### 7. "Give me a discount"
> Thanks for asking! Our best prices are on {{website}} right now. {{discount_policy}}
> If you're ordering {{bulk_min}} or more, tell me the items and I'll ask the team for a quote.

The AI never negotiates. A person may offer a one-time gesture (for example free shipping), and notes
it in the customer's profile.

### 8. Bulk / wholesale request
> That's great to hear! To prepare a quote, could you tell me: 1) which items, 2) quantities and sizes,
> 3) your city, 4) when you need them? I'll come back with a quote by {{reply_time}}.

Move the lead to the **Bulk** pipeline and tag it `bulk` (see `05-sales-process.md`).

### 9. Size exchange
> Of course! Which size would you like instead? I'll confirm it's in stock first. {{exchange_policy}}
> {{return_pickup}}

Check stock before promising. If the new size is out of stock, offer a refund or a different item.

### 10. Review request (after delivery)
> Hi {{name}}! We hope you're loving your order. 💛 Would you share a photo and a quick review?
> It helps other shoppers, and you'll get {{review_reward}} on your next order.

Only send this to customers who have confirmed their order was delivered without issues.

---

## Part 3: Escalation levels

| Level | Who | Examples | Response target |
|---|---|---|---|
| 0 | AI | Sizing, delivery times, order status | Instant |
| 1 | Team member | Exchanges, tracking issues, payment checks | within {{reply_time}} |
| 2 | Owner | Refunds outside policy, legal words, big customers, public complaints | same working day |

Write down every Level 2 case and its outcome. Patterns there tell you what to fix in your product,
packaging, or policy.
