import re
import unicodedata
from typing import Any, Dict, List

from loguru import logger


def _luhn_ok(digits: str) -> bool:
    total, alt = 0, False
    for ch in reversed(digits):
        d = int(ch)
        if alt:
            d *= 2
            if d > 9:
                d -= 9
        total += d
        alt = not alt
    return total % 10 == 0


_ZERO_WIDTH = dict.fromkeys(map(ord, "​‌‍⁠﻿­"), None)


def _normalize(text: str) -> str:
    """Defeats trivial evasion: unicode look-alikes, zero-width chars, odd spacing, case."""
    text = unicodedata.normalize("NFKC", text).translate(_ZERO_WIDTH)
    return re.sub(r"\s+", " ", text).casefold()


class DLPShield:
    """Inbound/outbound Data Loss Prevention and prompt-injection screening.

    This is a heuristic first line of defence. It reduces risk but cannot guarantee that
    hostile text is harmless: callers must still keep model output away from privileged
    actions (the Go kernel enforces policy independently of anything produced here).
    """

    _CARD = re.compile(r"(?<!\d)(?:\d[ -]?){12,18}\d(?!\d)")
    _AADHAAR = re.compile(r"(?<!\d)[2-9]\d{3}[ -]?\d{4}[ -]?\d{4}(?!\d)")
    _PAN = re.compile(r"\b[A-Z]{5}[0-9]{4}[A-Z]\b")
    _EMAIL = re.compile(r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b")
    _PHONE = re.compile(r"(?<!\d)(?:\+91[-\s]?|0)?[6-9]\d{9}(?!\d)")

    INJECTION_TRIGGERS = [
        r"ignore (?:all |any |the )?(?:previous|prior|above|earlier) (?:instructions|prompts?|rules)",
        r"disregard (?:all |any |the )?(?:previous|prior|above|earlier|your) (?:instructions|prompts?|rules)",
        r"forget (?:all |everything |your )?(?:previous|prior|above|earlier)? ?(?:instructions|rules)",
        r"system ?prompt",
        r"developer mode",
        r"jailbreak",
        r"you are now (?:in|a|an|the|my|dan|free|unrestricted|root|admin)\b",
        r"act as (?:an? )?(?:unrestricted|admin|root|system)",
        r"bypass (?:all )?(?:guardrails|safety|policy|policies|approval)",
        r"(?:reveal|print|show|leak) (?:your |the )?(?:secret|system|hidden|api)[ -]?(?:key|prompt|instructions)",
        r"<\s*/?\s*(?:script|iframe|untrusted_context|system|assistant)",
        r"(?:^|\n) ?(?:system|assistant) ?:",
        r"approve (?:this |the )?(?:refund|order|ticket|payment) (?:immediately|without)",
    ]
    _INJECTION_RES = [re.compile(p, re.IGNORECASE | re.DOTALL) for p in INJECTION_TRIGGERS]

    _ORIGIN_SAFE = re.compile(r"[^A-Za-z0-9_.-]")

    def _redact(self, text: str) -> (str, List[str]):
        found: List[str] = []

        def card_sub(m: "re.Match[str]") -> str:
            digits = re.sub(r"\D", "", m.group(0))
            if 13 <= len(digits) <= 19 and _luhn_ok(digits):
                if "CREDIT_CARD" not in found:
                    found.append("CREDIT_CARD")
                return "[REDACTED_CREDIT_CARD]"
            return m.group(0)

        text = self._CARD.sub(card_sub, text)
        for name, pattern in (
            ("AADHAAR", self._AADHAAR),
            ("PAN_IN", self._PAN),
            ("EMAIL", self._EMAIL),
            ("PHONE_IN", self._PHONE),
        ):
            text, n = pattern.subn(f"[REDACTED_{name}]", text)
            if n:
                found.append(name)
        return text, found

    def inspect_and_sanitize(self, content: str, origin_system: str = "web") -> Dict[str, Any]:
        content = content or ""
        origin = self._ORIGIN_SAFE.sub("_", origin_system or "unknown")[:64]

        sanitized, detected_pii = self._redact(content)

        normalized = _normalize(content)
        injection_detected = any(r.search(content) or r.search(normalized) for r in self._INJECTION_RES)
        if injection_detected:
            logger.warning(f"Prompt injection pattern detected from origin={origin}")

        if injection_detected:
            # Withhold hostile content entirely rather than passing it on in a wrapper.
            body = "[CONTENT WITHHELD: possible prompt injection]"
        else:
            # Neutralise markup so the content cannot close the quarantine tag.
            body = sanitized.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")

        quarantined = f'<untrusted_context origin="{origin}">\n{body}\n</untrusted_context>'
        return {
            "is_safe": not injection_detected,
            "sanitized_content": quarantined,
            "detected_pii_types": detected_pii,
            "injection_detected": injection_detected,
        }
