import json
import os
import time
from typing import Any, Dict, List, Optional

import redis
from loguru import logger

from config import DEV_WORKSPACE_ID, is_dev


class HotCacheUnavailable(RuntimeError):
    """Redis is unreachable and no development fixtures are enabled."""


def _dev_fixtures() -> Dict[str, Dict[str, Any]]:
    """Synthetic data for local development only (never loaded in production)."""
    return {
        f"order:{DEV_WORKSPACE_ID}:ord_1042": {
            "order_id": "ord_1042",
            "customer_name": "Test Customer",
            "customer_phone": "+919800000001",
            "status": "OUT_FOR_DELIVERY",
            "carrier": "Bluedart Express",
            "tracking_num": "BD-000000-IN",
            "eta": "Today by 4:00 PM",
            "amount_inr": "4200.00",
        },
        f"sku:{DEV_WORKSPACE_ID}:SKU-POLO-BLUE-M": {
            "sku": "SKU-POLO-BLUE-M",
            "name": "Classic Polo Blue (M)",
            "in_stock": 28,
            "price_inr": "899.00",
        },
    }


class VoiceHotCache:
    """Low-latency Redis operational cache reader for the voice talker."""

    RECONNECT_INTERVAL_S = 5.0

    def __init__(self, redis_url: Optional[str] = None):
        self._url = redis_url or os.getenv("REDIS_URL", "redis://127.0.0.1:6379/0")
        self._local_cache: Dict[str, Dict[str, Any]] = _dev_fixtures() if is_dev() else {}
        self._r: Optional[redis.Redis] = None
        self._last_attempt = 0.0
        self._connect()

    # -- connection handling -------------------------------------------------
    def _connect(self) -> None:
        self._last_attempt = time.monotonic()
        try:
            client = redis.from_url(
                self._url, decode_responses=True, socket_connect_timeout=0.2, socket_timeout=0.2
            )
            client.ping()
            self._r = client
        except Exception as e:  # noqa: BLE001
            self._r = None
            logger.warning(f"Redis hot cache unreachable ({type(e).__name__}); dev fixtures={'on' if is_dev() else 'off'}")

    @property
    def is_connected(self) -> bool:
        return self._r is not None

    def _client(self) -> Optional[redis.Redis]:
        if self._r is None and time.monotonic() - self._last_attempt > self.RECONNECT_INTERVAL_S:
            self._connect()
        return self._r

    # -- reads ---------------------------------------------------------------
    def _get(self, key: str) -> Optional[Dict[str, Any]]:
        client = self._client()
        if client is not None:
            try:
                data = client.get(key)
                return json.loads(data) if data else None
            except Exception as e:  # noqa: BLE001
                logger.warning(f"Redis get failed ({type(e).__name__})")
                self._r = None
        if is_dev():
            return self._local_cache.get(key)
        raise HotCacheUnavailable("hot cache is unavailable")

    def get_order_status(self, workspace_id: str, order_id: str) -> Optional[Dict[str, Any]]:
        return self._get(f"order:{workspace_id}:{order_id}")

    def get_sku_stock(self, workspace_id: str, sku: str) -> Optional[Dict[str, Any]]:
        return self._get(f"sku:{workspace_id}:{sku}")

    def find_sku(self, workspace_id: str, words: List[str]) -> Optional[Dict[str, Any]]:
        """Finds a SKU whose name contains every given word (bounded scan)."""
        words = [w.lower() for w in words if len(w) > 2]
        if not words:
            return None
        prefix = f"sku:{workspace_id}:"
        client = self._client()
        if client is not None:
            try:
                for i, key in enumerate(client.scan_iter(match=prefix + "*", count=100)):
                    if i >= 500:
                        break
                    raw = client.get(key)
                    item = json.loads(raw) if raw else None
                    if item and all(w in item.get("name", "").lower() for w in words):
                        return item
                return None
            except Exception as e:  # noqa: BLE001
                logger.warning(f"Redis scan failed ({type(e).__name__})")
                self._r = None
        if is_dev():
            for key, item in self._local_cache.items():
                if key.startswith(prefix) and all(w in item.get("name", "").lower() for w in words):
                    return item
            return None
        raise HotCacheUnavailable("hot cache is unavailable")
