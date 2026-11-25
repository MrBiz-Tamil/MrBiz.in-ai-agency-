import re
from typing import Any, Dict, Optional, Tuple

from loguru import logger

from voice.hot_cache import HotCacheUnavailable, VoiceHotCache

# An order reference must contain at least one digit, so words like "status" are never captured.
_ORDER_RE = re.compile(
    r"\b(?:order|ord)\b[\s#:_-]*(?:number|no\.?|id)?[\s#:]*((?:ord_)?[A-Za-z0-9_-]*\d[A-Za-z0-9_-]*)",
    re.IGNORECASE,
)
_SKU_RE = re.compile(r"\bSKU-[A-Z0-9-]+\b", re.IGNORECASE)
_STOPWORDS = {"the", "and", "for", "have", "you", "any", "stock", "price", "available", "much", "how",
              "what", "does", "cost", "this", "that", "with", "shirt", "your", "are", "there", "in"}


def normalize_phone(phone: str) -> str:
    digits = re.sub(r"\D", "", phone or "")
    return digits[-10:] if len(digits) >= 10 else digits


def mask_phone(phone: str) -> str:
    return "***" + normalize_phone(phone)[-4:] if phone else "unknown"


def extract_order_id(utterance: str) -> Optional[str]:
    m = _ORDER_RE.search(utterance)
    if not m:
        return None
    ref = m.group(1).strip("_-")
    if ref.isdigit():
        ref = f"ord_{ref}"
    return ref.lower() if ref.lower().startswith("ord_") else ref


class SynchronousTalker:
    """Synchronous talker for live phone and WebRTC interactions (cache reads only)."""

    UNAVAILABLE = "I'm having trouble looking that up right now. Let me connect you with a team member."
    NOT_FOUND = "I couldn't find an order matching that for this phone number. Could you confirm the order number?"

    def __init__(self, cache: VoiceHotCache):
        self.cache = cache

    def _owned_order(self, workspace_id: str, order_id: str, caller_phone: str) -> Optional[Dict[str, Any]]:
        """Returns the order only if it belongs to the calling phone number."""
        order = self.cache.get_order_status(workspace_id, order_id)
        if not order:
            return None
        owner = normalize_phone(order.get("customer_phone", ""))
        if not owner or owner != normalize_phone(caller_phone):
            return None  # same answer as "not found": do not reveal that the order exists
        return order

    def process_utterance(
        self, workspace_id: str, caller_phone: str, user_transcript: str
    ) -> Tuple[str, bool, Optional[Dict[str, Any]]]:
        """
        Returns (spoken_reply, requires_mutation, mutation_payload).
        Transcripts and phone numbers are not logged; only a masked number and a length.
        """
        logger.info(f"Talker utterance from {mask_phone(caller_phone)} ({len(user_transcript)} chars)")
        text = user_transcript.lower()
        try:
            return self._process(workspace_id, caller_phone, user_transcript, text)
        except HotCacheUnavailable:
            return self.UNAVAILABLE, False, None

    def _process(self, workspace_id: str, caller_phone: str, transcript: str, text: str):
        order_id = extract_order_id(transcript)

        # 1. Address change intent (checked first: it also mentions the order)
        if "change address" in text or "update delivery" in text or "change the address" in text:
            if not order_id:
                return "Sure. Which order number is the address change for?", False, None
            order = self._owned_order(workspace_id, order_id, caller_phone)
            if not order:
                return self.NOT_FOUND, False, None
            spoken = (
                "I've recorded your request to change the delivery address. "
                "It needs verification first, and you'll get a confirmation once it's done."
            )
            return spoken, True, {
                "action": "order.update_shipping_address",
                "caller_phone": caller_phone,
                "requested_change": transcript,
                "order_id": order["order_id"],
            }

        # 2. Order status (cache read only)
        if order_id or "where is my order" in text:
            if not order_id:
                return "Happy to check. What's your order number?", False, None
            order = self._owned_order(workspace_id, order_id, caller_phone)
            if not order:
                return self.NOT_FOUND, False, None
            spoken = (
                f"Your order {order['order_id']} is {order['status'].replace('_', ' ').lower()} "
                f"with {order['carrier']}. Estimated arrival is {order['eta']}."
            )
            return spoken, False, None

        # 3. Stock / price lookup
        if any(w in text for w in ("stock", "available", "price")):
            sku_match = _SKU_RE.search(transcript)
            if sku_match:
                sku = self.cache.get_sku_stock(workspace_id, sku_match.group(0).upper())
            else:
                words = [w for w in re.findall(r"[a-z]+", text) if w not in _STOPWORDS]
                sku = self.cache.find_sku(workspace_id, words)
            if sku:
                return (
                    f"We have {sku['in_stock']} units of {sku['name']} in stock, priced at ₹{sku['price_inr']}.",
                    False,
                    None,
                )
            return "Which product are you asking about? You can give me its name or SKU code.", False, None

        # 4. Fallback
        return "Thanks for calling Mrbiz support. How can I assist with your order, delivery, or inventory today?", False, None
