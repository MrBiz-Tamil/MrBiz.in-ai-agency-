import json

import httpx
import pytest

from compiler.policy_compiler import PolicyCompileError, PolicyCompiler, validate_rule_ast
from config import DEV_WORKSPACE_ID
from router.llm_router import LLMRouter, LLMUnavailableError, ModelTier


class StubRouter:
    """Router returning a canned completion."""

    def __init__(self, content=None, mock=False):
        self.content, self.mock = content, mock

    async def complete(self, **kwargs):
        return {"content": self.content, "mock": self.mock, "model": "stub"}


async def compile_(text, router=None):
    return await PolicyCompiler(router or StubRouter(mock=True)).compile(text, DEV_WORKSPACE_ID)


async def test_refund_policy_compiles_deterministically():
    a = await compile_("Pause any refund above ₹1,000 for manager approval")
    b = await compile_("Pause any refund above ₹1,000 for manager approval")
    assert a["rule_id"] == b["rule_id"] and a["rule_id"].startswith("pol_")
    ast = json.loads(a["compiled_ast_json"])
    assert a["trigger_event"] == "order.refund_requested"
    assert ast["conditions"] == [{"field": "refund_amount", "operator": "GREATER_THAN", "threshold": 1000}]
    validate_rule_ast(ast)


async def test_discount_policy_and_phrasing_variants():
    r = await compile_("any discount exceeding 12.5% needs approval")
    assert r["trigger_event"] == "order.discount_requested"
    assert json.loads(r["compiled_ast_json"])["conditions"][0]["threshold"] == 12.5
    r = await compile_("refunds more than 2500 INR must be reviewed")
    assert json.loads(r["compiled_ast_json"])["conditions"][0]["threshold"] == 2500


async def test_rule_ids_differ_per_workspace_and_text():
    a = await PolicyCompiler(StubRouter(mock=True)).compile("refund above 500", DEV_WORKSPACE_ID)
    b = await PolicyCompiler(StubRouter(mock=True)).compile("refund above 500", "0b8f3d5e-2f7a-4c1b-8d9e-1a2b3c4d5e6f")
    c = await compile_("refund above 600")
    assert len({a["rule_id"], b["rule_id"], c["rule_id"]}) == 3


async def test_combined_policy_and_foreign_currency_rejected():
    with pytest.raises(PolicyCompileError):
        await compile_("Pause any refund above 1000 or discount above 12%")
    with pytest.raises(PolicyCompileError):
        await compile_("refund above $500 needs approval")
    with pytest.raises(PolicyCompileError):
        await compile_("")


async def test_mock_llm_output_is_refused_not_trusted():
    with pytest.raises(PolicyCompileError):
        await compile_("Hold orders from new customers over a large amount", StubRouter(mock=True))


async def test_llm_output_is_validated():
    good = json.dumps({
        "trigger_event": "order.refund_requested",
        "conditions": [{"field": "refund_amount", "operator": "GREATER_THAN", "threshold": 300}],
        "action": "SUSPEND_FOR_APPROVAL",
        "assigned_role": "Finance",
        "rule_id": "evil_id",
    })
    r = await compile_("complex policy text", StubRouter(good))
    assert r["rule_id"].startswith("pol_") and r["rule_id"] != "evil_id"
    assert r["assigned_approval_role"] == "Finance"

    bad_cases = [
        {"trigger_event": "order.refund_requested", "conditions": [{"field": "admin", "operator": "GREATER_THAN", "threshold": 1}], "action": "SUSPEND_FOR_APPROVAL"},
        {"trigger_event": "order.refund_requested", "conditions": [{"field": "refund_amount", "operator": "EVAL", "threshold": 1}], "action": "SUSPEND_FOR_APPROVAL"},
        {"trigger_event": "order.refund_requested", "conditions": [{"field": "refund_amount", "operator": "GREATER_THAN", "threshold": -5}], "action": "SUSPEND_FOR_APPROVAL"},
        {"trigger_event": "order.refund_requested", "conditions": [{"field": "refund_amount", "operator": "GREATER_THAN", "threshold": 5}], "action": "APPROVE_ALL"},
        {"trigger_event": "order.refund_requested", "conditions": [{"field": "refund_amount", "operator": "GREATER_THAN", "threshold": 5}], "action": "SUSPEND_FOR_APPROVAL", "assigned_role": "Hacker"},
        {"trigger_event": "order.discount_requested", "conditions": [{"field": "refund_amount", "operator": "GREATER_THAN", "threshold": 5}], "action": "SUSPEND_FOR_APPROVAL"},
        {},
    ]
    for bad in bad_cases:
        with pytest.raises(PolicyCompileError):
            await compile_("complex policy text", StubRouter(json.dumps(bad)))
    with pytest.raises(PolicyCompileError):
        await compile_("complex policy text", StubRouter("not json"))


# ---- router ---------------------------------------------------------------

async def test_router_fails_closed_in_production(monkeypatch):
    monkeypatch.setenv("ABOS_ENV", "production")
    monkeypatch.delenv("ABOS_ALLOW_MOCK_LLM", raising=False)
    with pytest.raises(LLMUnavailableError):
        await LLMRouter().complete("hi")


async def test_router_mock_is_flagged_in_dev():
    res = await LLMRouter().complete("hi", expected_json=True)
    assert res["mock"] is True and json.loads(res["content"])["mock"] is True


async def test_gemini_key_sent_in_header_not_url(monkeypatch):
    monkeypatch.setenv("ABOS_ENV", "production")
    monkeypatch.setenv("GEMINI_API_KEY", "SECRET-KEY")
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"], seen["headers"], seen["json"] = str(request.url), request.headers, json.loads(request.content)
        return httpx.Response(200, json={"candidates": [{"content": {"parts": [{"text": "ok"}]}}]})

    router = LLMRouter(client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    res = await router.complete("hello", system_instruction="be brief", temperature=0.1)
    assert res["content"] == "ok" and res["mock"] is False
    assert "SECRET-KEY" not in seen["url"] and seen["headers"]["x-goog-api-key"] == "SECRET-KEY"
    assert seen["json"]["systemInstruction"]["parts"][0]["text"] == "be brief"
    assert seen["json"]["generationConfig"]["temperature"] == 0.1


async def test_router_retries_then_falls_back(monkeypatch):
    monkeypatch.setenv("ABOS_ENV", "production")
    monkeypatch.setenv("GEMINI_API_KEY", "g")
    monkeypatch.setenv("OPENAI_API_KEY", "o")
    monkeypatch.setattr("router.llm_router.asyncio.sleep", lambda s: _noop())
    calls = {"gemini": 0, "openai": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        if "googleapis" in str(request.url):
            calls["gemini"] += 1
            return httpx.Response(503)
        calls["openai"] += 1
        return httpx.Response(200, json={"choices": [{"message": {"content": "from-openai"}}]})

    router = LLMRouter(client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    res = await router.complete("hi", tier=ModelTier.FLASH)
    assert res["content"] == "from-openai"
    assert calls == {"gemini": 3, "openai": 1}


async def test_router_all_providers_failing_raises_in_production(monkeypatch):
    monkeypatch.setenv("ABOS_ENV", "production")
    monkeypatch.setenv("OPENAI_API_KEY", "o")
    monkeypatch.setattr("router.llm_router.asyncio.sleep", lambda s: _noop())
    router = LLMRouter(client=httpx.AsyncClient(transport=httpx.MockTransport(lambda r: httpx.Response(401))))
    with pytest.raises(LLMUnavailableError):
        await router.complete("hi")


async def _noop():
    return None
