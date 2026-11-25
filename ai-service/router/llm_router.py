import asyncio
import json
import logging
import os
from enum import Enum
from typing import Any, Dict, List, Optional

import httpx
from loguru import logger

from config import is_dev

# httpx logs full request URLs at INFO; keep credentials and prompts out of logs.
logging.getLogger("httpx").setLevel(logging.WARNING)
logging.getLogger("httpcore").setLevel(logging.WARNING)


class ModelTier(str, Enum):
    FLASH = "FLASH"         # Low latency: Gemini 2.5 Flash / Claude Haiku / GPT-4o-mini
    FRONTIER = "FRONTIER"   # High reasoning: Gemini 2.5 Pro / Claude Sonnet / GPT-4o


class LLMUnavailableError(RuntimeError):
    """No configured provider could serve the request."""


_RETRYABLE_STATUS = {408, 409, 425, 429, 500, 502, 503, 504}


class LLMRouter:
    """Model-agnostic LLM router with tiered routing, retries and provider fallback.

    It is fail-closed: unless ABOS_ENV=dev (or ABOS_ALLOW_MOCK_LLM=1) a request that no
    provider can serve raises LLMUnavailableError instead of returning invented output.
    Mock responses are always flagged with ``"mock": True``.
    """

    MAX_ATTEMPTS = 3

    def __init__(self, client: Optional[httpx.AsyncClient] = None):
        self.gemini_api_key = os.getenv("GEMINI_API_KEY", "")
        self.openai_api_key = os.getenv("OPENAI_API_KEY", "")
        self.anthropic_api_key = os.getenv("ANTHROPIC_API_KEY", "")
        self.allow_mock = is_dev() or os.getenv("ABOS_ALLOW_MOCK_LLM") == "1"
        self.client = client or httpx.AsyncClient(timeout=30.0)

    async def aclose(self) -> None:
        await self.client.aclose()

    def _providers(self, tier: ModelTier) -> List[Any]:
        flash = tier == ModelTier.FLASH
        providers: List[Any] = []
        if self.gemini_api_key:
            providers.append(("gemini-2.5-flash" if flash else "gemini-2.5-pro", self._call_gemini))
        if self.anthropic_api_key:
            providers.append(("claude-haiku-5-5" if flash else "claude-sonnet-5-5", self._call_anthropic))
        if self.openai_api_key:
            providers.append(("gpt-4o-mini" if flash else "gpt-4o", self._call_openai))
        return providers

    async def complete(
        self,
        prompt: str,
        system_instruction: str = "",
        tier: ModelTier = ModelTier.FLASH,
        temperature: float = 0.2,
        expected_json: bool = False,
    ) -> Dict[str, Any]:
        """Routes the prompt to the first healthy provider for the tier."""
        logger.info(f"Routing completion request | Tier: {tier.value} | Temp: {temperature}")

        providers = self._providers(tier)
        last_error: Optional[Exception] = None
        for model, call in providers:
            try:
                return await self._with_retries(call, model, prompt, system_instruction, temperature, expected_json)
            except Exception as e:  # noqa: BLE001 - provider failures are expected, try the next
                last_error = e
                logger.warning(f"Provider call for {model} failed: {type(e).__name__}")

        if self.allow_mock:
            if providers:
                logger.warning("All providers failed; returning flagged MOCK completion (dev mode)")
            return self._mock_completion(tier, expected_json)

        if not providers:
            raise LLMUnavailableError("No LLM provider is configured (set GEMINI_API_KEY, ANTHROPIC_API_KEY or OPENAI_API_KEY)")
        raise LLMUnavailableError("All configured LLM providers failed") from last_error

    async def _with_retries(self, call, model, prompt, system, temperature, expected_json) -> Dict[str, Any]:
        delay = 0.5
        for attempt in range(1, self.MAX_ATTEMPTS + 1):
            try:
                return await call(model, prompt, system, temperature, expected_json)
            except httpx.HTTPStatusError as e:
                if e.response.status_code not in _RETRYABLE_STATUS or attempt == self.MAX_ATTEMPTS:
                    raise
            except (httpx.TransportError, httpx.TimeoutException):
                if attempt == self.MAX_ATTEMPTS:
                    raise
            await asyncio.sleep(delay)
            delay *= 2
        raise AssertionError("unreachable")

    async def _call_gemini(self, model: str, prompt: str, system: str, temperature: float, expected_json: bool) -> Dict[str, Any]:
        url = f"https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent"
        payload: Dict[str, Any] = {
            "contents": [{"role": "user", "parts": [{"text": prompt}]}],
            "generationConfig": {"temperature": temperature},
        }
        if system:
            payload["systemInstruction"] = {"parts": [{"text": system}]}
        if expected_json:
            payload["generationConfig"]["responseMimeType"] = "application/json"

        # API key travels in a header, never in the URL (URLs end up in logs and traces).
        resp = await self.client.post(url, json=payload, headers={"x-goog-api-key": self.gemini_api_key})
        resp.raise_for_status()
        data = resp.json()
        text = data["candidates"][0]["content"]["parts"][0]["text"]
        return {"content": text, "model": model, "cached": False, "mock": False}

    async def _call_openai(self, model: str, prompt: str, system: str, temperature: float, expected_json: bool) -> Dict[str, Any]:
        messages = []
        if system:
            messages.append({"role": "system", "content": system})
        messages.append({"role": "user", "content": prompt})
        payload: Dict[str, Any] = {"model": model, "messages": messages, "temperature": temperature}
        if expected_json:
            payload["response_format"] = {"type": "json_object"}

        resp = await self.client.post(
            "https://api.openai.com/v1/chat/completions",
            json=payload,
            headers={"Authorization": f"Bearer {self.openai_api_key}"},
        )
        resp.raise_for_status()
        text = resp.json()["choices"][0]["message"]["content"]
        return {"content": text, "model": model, "cached": False, "mock": False}

    async def _call_anthropic(self, model: str, prompt: str, system: str, temperature: float, expected_json: bool) -> Dict[str, Any]:
        payload: Dict[str, Any] = {
            "model": model,
            "max_tokens": 2048,
            "temperature": temperature,
            "messages": [{"role": "user", "content": prompt}],
        }
        if system:
            payload["system"] = system
        resp = await self.client.post(
            "https://api.anthropic.com/v1/messages",
            json=payload,
            headers={"x-api-key": self.anthropic_api_key, "anthropic-version": "2023-06-01"},
        )
        resp.raise_for_status()
        blocks = resp.json()["content"]
        text = "".join(b.get("text", "") for b in blocks if b.get("type") == "text")
        return {"content": text, "model": model, "cached": False, "mock": False}

    @staticmethod
    def _mock_completion(tier: ModelTier, expected_json: bool) -> Dict[str, Any]:
        """Offline development stand-in. Always flagged so callers can refuse to act on it."""
        if expected_json:
            content = json.dumps({"mock": True, "note": f"Simulated {tier.value} output"})
        else:
            content = f"[Simulated {tier.value} Tier Output]"
        return {"content": content, "model": f"mock-{tier.value.lower()}", "cached": True, "mock": True}
