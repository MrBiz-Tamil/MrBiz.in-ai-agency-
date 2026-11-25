# FAQ Answers (36)

How to use: load each answer into your inbox as a saved reply, or paste the whole file into your AI
assistant's knowledge. Replace every `{{placeholder}}` using the Brand Facts Sheet in `00-START-HERE.md`.

Tag meaning:
- **[AI]**: safe for the AI to answer on its own.
- **[AI → CONFIRM]**: the AI answers, but must check live data first (stock, order status).
- **[HUMAN]**: the AI collects details, then hands over to a person (see `04-chat-scripts.md`).

Style rules: short messages, one idea each, friendly, no long paragraphs. Always end with a next step
("Want me to check your size?"). Never invent a fact. If a fact is missing, say "Let me check with the
team and come back to you" and hand over.

---

## A. Sizing and fit (6)

**1. "What size should I order?" [AI]**
Happy to help! 😊 Tell me your usual size in a brand you wear, or your height and weight, and I'll
suggest a size. Our size chart is here: {{size_chart_link}}. {{fit_note}}

**2. "I'm between two sizes. Which one?" [AI]**
If you prefer a relaxed fit, go one size up. For a snug fit, pick the smaller size. {{fit_note}}
If it still doesn't fit, {{exchange_policy}}. Which fit do you like?

**3. "What are the measurements of this item?" [AI → CONFIRM]**
Great question. The exact measurements for this item are on its product page under "Size chart":
{{website}}. If you tell me the item name I'll send the numbers for your size.

**4. "Is it true to size?" [AI]**
{{fit_note}} If you share your usual size, I can tell you what most customers with that size choose.

**5. "Do you have plus sizes / kids sizes?" [AI → CONFIRM]**
Let me check what we have in that range. Which item are you looking at, and what size do you need?

**6. "Will it shrink after washing?" [AI]**
Follow the care label and our care tip: {{fabric_care}}. Washing as instructed keeps the fit.

## B. Product and fabric (5)

**7. "What is the fabric?" [AI → CONFIRM]**
The fabric details are on the product page: {{website}}. Tell me which item and I'll share the fabric
and how it feels (breathable, stretchy, etc.).

**8. "Is it see-through?" [AI → CONFIRM]**
For light colours, we mention opacity on the product page. Which item and colour? I'll check the notes
for you.

**9. "Is the colour the same as in the photos?" [AI]**
We photograph in natural light and don't over-edit, but screens vary slightly. If the colour looks
different when it arrives, {{exchange_policy}}.

**10. "How do I wash it?" [AI]**
{{fabric_care}} Each item also has a care label inside.

**11. "Can I see more photos or a video?" [HUMAN or AI]**
Of course! Which item? I'll send more photos. If a video is available I'll share it.

## C. Availability and restock (4)

**12. "Is this available in my size?" [AI → CONFIRM]**
Let me check live stock. Which item and size? ⏳ (AI: look up stock, never guess.)

**13. "When will it be back in stock?" [AI]**
{{restock_policy}} Want me to notify you on WhatsApp as soon as your size is back? Reply **YES** and
I'll add you to the list.

**14. "Will you launch more designs like this?" [AI]**
New drops come regularly. Follow us on Instagram for launches, and reply **YES** if you'd like
WhatsApp alerts for new arrivals.

**15. "Can you make this in a different colour or size?" [HUMAN]**
Thanks for the idea! I'll pass it to the team. Which item and what would you like changed?

## D. Ordering and payment (6)

**16. "How do I place an order?" [AI]**
Easiest way: open {{website}}, add your item and size to the bag, and check out. Want me to send the
direct link to the item?

**17. "What payment options do you have?" [AI]**
{{payment_modes}}

**18. "Is cash on delivery available?" [AI]**
{{cod_available}}. {{cod_fee}} For COD orders we may send a quick WhatsApp to confirm your address
before dispatch.

**19. "Is my payment safe?" [AI]**
Yes. Payments go through a secure payment gateway; we never see or store your card details.

**20. "I paid but didn't get confirmation." [HUMAN]**
Sorry about that! Please share your phone number or the payment screenshot (hide any card number) and
I'll check right away. (Collect: order number or phone, amount, time of payment.)

**21. "Can I change my address / size / item after ordering?" [AI → CONFIRM]**
If your order hasn't been dispatched, we can usually change it. Please share your order number and
what you'd like to change. (Check dispatch status; if already shipped, hand over to a person.)

## E. Delivery and tracking (6)

**22. "How long does delivery take?" [AI]**
Orders dispatch {{dispatch_time}}. Delivery usually takes {{delivery_days_metro}} to metro cities and
{{delivery_days_other}} elsewhere. Share your pincode and I'll give you a better idea.

**23. "What are the shipping charges?" [AI]**
{{shipping_fee}}

**24. "Where is my order?" [AI → CONFIRM]**
Let me check! Please share your order number (it looks like #1234) or the phone number you ordered
with. (AI: fetch the status; share only status and tracking link, never full address or payment details.)

**25. "The tracking hasn't updated for days." [HUMAN]**
Sorry for the wait, I understand it's frustrating. I'm checking with our courier partner now and will
update you by {{reply_time}}. (Collect: order number, tracking number. Raise with the courier.)

**26. "Do you deliver to my pincode / abroad?" [AI → CONFIRM]**
Let me check. Please share your pincode (or country). (Do not promise abroad delivery unless you
actually offer it.)

**27. "Can you deliver by a specific date?" [AI]**
I can't guarantee an exact date, but our usual timeline is {{delivery_days_metro}} (metro) and
{{delivery_days_other}} (other areas). Tell me your date and pincode; if it's tight I'll ask the team
whether we can help.

## F. Returns, exchanges and refunds (6)

**28. "What is your return policy?" [AI]**
You can request a return within {{return_window}} of delivery. Items should be {{return_conditions}}.
{{non_returnable}} Want to start one? Send me your order number.

**29. "How do I exchange for a different size?" [AI → CONFIRM]**
Easy! {{exchange_policy}} Send me your order number and the size you'd like, and I'll check it's in
stock before we arrange it. {{return_pickup}}

**30. "When will I get my refund?" [AI]**
Refunds go to {{refund_method}}, usually within {{refund_time}}. If it's been longer, share your order
number and I'll check on it. (If beyond the window: [HUMAN].)

**31. "I received the wrong / damaged item." [HUMAN]**
I'm really sorry about that! 🙏 Please send your order number and a clear photo of the item and the
parcel label. We'll make this right as quickly as we can. (Hand over to a person; see script.)

**32. "I want to return after the return period." [HUMAN]**
Thanks for telling me. Our policy is {{return_window}}, so let me check with the team whether we can
help in your case. (Never promise; a person decides.)

**33. "Refund for a COD order?" [AI]**
For COD orders we refund to your bank account or UPI. Once your return is received and checked, send
us the details and the refund is processed within {{refund_time}}. (Never ask for passwords or OTPs.
Bank/UPI details only through the secure form your tool provides, if you have one.)

## G. Offers, trust and other (3)

**34. "Any discount or coupon code?" [AI]**
Our current offers are on {{website}}. {{discount_policy}} Want me to share the newest code that is
live right now? (AI: only codes listed publicly; never invent or stretch a discount beyond
{{max_discount_ai}}.)

**35. "Is this original / are you a genuine seller?" [AI]**
Yes, {{brand}} designs and sells its own clothing. You can see real customer photos and reviews on our
page, and every order comes with an invoice. Anything you'd like to know before ordering?

**36. "Do you do bulk or wholesale orders?" [HUMAN]**
Yes, we do for orders of {{bulk_min}} or more. Could you tell me the items, quantities and your city?
I'll ask the team to share a quote. (Hand over to a person; see script.)
