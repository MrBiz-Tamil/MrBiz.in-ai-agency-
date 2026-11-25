import asyncio
import hashlib
import json
import uuid
from typing import Any, Dict, Optional

from loguru import logger

from config import is_dev
from voice.kernel_client import KernelClient, KernelUnavailable


class MutationDispatchUnavailable(RuntimeError):
    """The mutation could not be handed to the kernel, so nothing was submitted."""


# Kernel ticket states that mean "the request is safely recorded and being handled".
_ACCEPTED = {"QUEUED", "RUNNING", "PENDING_APPROVAL", "CONFLICT_HOLD", "COMPLETED"}


def idempotency_key(workspace_id: str, call_id: str, payload: Dict[str, Any]) -> str:
    """Stable per (workspace, call, action, order): a retry of the same request never duplicates it."""
    basis = f"{workspace_id}|{call_id}|{payload.get('action')}|{payload.get('order_id')}|{payload.get('requested_change')}"
    return "voice_" + hashlib.sha256(basis.encode()).hexdigest()[:40]


class AsynchronousReasoner:
    """Hands mutation intents from the talker to the Go kernel as execution tickets.

    With ABOS_KERNEL_ADDR configured it calls KernelExecutionService.CreateTicket. Without
    a kernel it refuses (callers must then tell the customer nothing was submitted), except
    in development where it returns a clearly flagged simulated ticket.
    """

    def __init__(self, kernel: Optional[KernelClient] = None):
        self.kernel = kernel if kernel is not None else KernelClient.from_env()

    async def dispatch_erp_mutation(
        self, workspace_id: str, mutation_payload: Dict[str, Any], call_id: str = "", key: str = ""
    ) -> Dict[str, Any]:
        action = mutation_payload.get("action", "unknown")

        if self.kernel is not None:
            key = key or idempotency_key(workspace_id, call_id, mutation_payload)
            try:
                res = await self.kernel.create_ticket(
                    workspace_id=workspace_id,
                    idempotency_key=key,
                    action_type=action,
                    payload_json=json.dumps(mutation_payload),
                    priority=3,
                )
            except KernelUnavailable as e:
                raise MutationDispatchUnavailable(str(e)) from e
            if res["status"] not in _ACCEPTED:
                raise MutationDispatchUnavailable(f"kernel ended the ticket in state {res['status']}")
            return {"ticket_id": res["ticket_id"], "status": res["status"], "action": action, "idempotency_key": key, "simulated": False}

        if not is_dev():
            raise MutationDispatchUnavailable("no kernel is configured")

        ticket_id = f"tkt_sim_{uuid.uuid4().hex[:8]}"
        logger.info(f"[dev] simulated ticket {ticket_id} for action {action}")
        await asyncio.sleep(0.01)
        return {"ticket_id": ticket_id, "status": "QUEUED", "action": action, "idempotency_key": f"idemp_{ticket_id}", "simulated": True}
