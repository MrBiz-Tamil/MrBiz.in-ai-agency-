import pytest

from config import DEV_WORKSPACE_ID
from voice.hot_cache import HotCacheUnavailable, VoiceHotCache
from voice.reasoner import AsynchronousReasoner, MutationDispatchUnavailable
from voice.talker import SynchronousTalker, extract_order_id

OWNER_PHONE = "+919800000001"
OTHER_PHONE = "+919811111111"


@pytest.fixture
def talker():
    return SynchronousTalker(VoiceHotCache())


def test_order_lookup_for_owner(talker):
    spoken, needs_mutation, data = talker.process_utterance(DEV_WORKSPACE_ID, OWNER_PHONE, "Hi, where is my order #ord_1042?")
    assert "ord_1042" in spoken and "Bluedart" in spoken
    assert needs_mutation is False and data is None


def test_order_lookup_numeric_reference(talker):
    spoken, *_ = talker.process_utterance(DEV_WORKSPACE_ID, OWNER_PHONE, "status of order 1042 please")
    assert "ord_1042" in spoken


def test_order_not_disclosed_to_other_callers(talker):
    spoken, needs_mutation, _ = talker.process_utterance(DEV_WORKSPACE_ID, OTHER_PHONE, "where is my order #ord_1042?")
    assert "Bluedart" not in spoken and "ord_1042" not in spoken.replace("order number", "")
    assert needs_mutation is False
    missing, *_ = talker.process_utterance(DEV_WORKSPACE_ID, OTHER_PHONE, "where is my order #ord_9999?")
    assert spoken == missing  # cannot tell "exists but not yours" from "does not exist"


def test_no_default_order_when_none_given(talker):
    spoken, needs_mutation, _ = talker.process_utterance(DEV_WORKSPACE_ID, OWNER_PHONE, "where is my order")
    assert "order number" in spoken.lower()
    assert needs_mutation is False


def test_stock_lookup_by_name(talker):
    spoken, needs_mutation, _ = talker.process_utterance(
        DEV_WORKSPACE_ID, OWNER_PHONE, "Is the classic polo blue shirt available in stock?"
    )
    assert "28" in spoken and "stock" in spoken.lower()
    assert needs_mutation is False


def test_address_change_targets_the_named_order(talker):
    spoken, needs_mutation, data = talker.process_utterance(
        DEV_WORKSPACE_ID, OWNER_PHONE, "I need to change address for order #ord_1042 to Warehouse 4"
    )
    assert needs_mutation is True
    assert data["action"] == "order.update_shipping_address"
    assert data["order_id"] == "ord_1042"
    assert "recorded" in spoken.lower() and "verification" in spoken.lower()


def test_address_change_requires_order_and_ownership(talker):
    spoken, needs_mutation, data = talker.process_utterance(DEV_WORKSPACE_ID, OWNER_PHONE, "please change address to Warehouse 4")
    assert needs_mutation is False and data is None and "order number" in spoken.lower()
    _, needs_mutation, data = talker.process_utterance(DEV_WORKSPACE_ID, OTHER_PHONE, "change address for order ord_1042")
    assert needs_mutation is False and data is None


def test_extract_order_id_ignores_plain_words():
    assert extract_order_id("what is the status of my order") is None
    assert extract_order_id("order #1042") == "ord_1042"


def test_cache_miss_with_live_redis_does_not_crash(monkeypatch):
    class FakeRedis:
        def get(self, key):
            return None

    cache = VoiceHotCache()
    cache._r = FakeRedis()
    assert cache.get_order_status(DEV_WORKSPACE_ID, "ord_404") is None  # used to raise AttributeError


def test_production_never_serves_fixtures(monkeypatch):
    monkeypatch.setenv("ABOS_ENV", "production")
    cache = VoiceHotCache()
    assert cache._local_cache == {}
    with pytest.raises(HotCacheUnavailable):
        cache.get_order_status(DEV_WORKSPACE_ID, "ord_1042")
    spoken, needs_mutation, _ = SynchronousTalker(cache).process_utterance(DEV_WORKSPACE_ID, OWNER_PHONE, "where is order 1042")
    assert "trouble" in spoken.lower() and needs_mutation is False


async def test_reasoner_refuses_in_production(monkeypatch):
    monkeypatch.setenv("ABOS_ENV", "production")
    with pytest.raises(MutationDispatchUnavailable):
        await AsynchronousReasoner().dispatch_erp_mutation(DEV_WORKSPACE_ID, {"action": "x"})


async def test_reasoner_dev_ticket_is_flagged_simulated():
    res = await AsynchronousReasoner().dispatch_erp_mutation(DEV_WORKSPACE_ID, {"action": "x"})
    assert res["simulated"] is True
