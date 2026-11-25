"""Async client for the Go kernel's KernelExecutionService (HMAC-authenticated)."""
import os
from typing import Any, Dict, Optional

import grpc

from gen.abos.kernel.v1 import kernel_service_pb2, kernel_service_pb2_grpc
from security.grpc_auth import make_token


class KernelUnavailable(RuntimeError):
    """The kernel could not be reached or refused the request."""


class KernelClient:
    def __init__(self, addr: str, secret: str = "", credentials: Optional[grpc.ChannelCredentials] = None, timeout: float = 5.0):
        self._secret = secret
        self._timeout = timeout
        self._channel = (
            grpc.aio.secure_channel(addr, credentials) if credentials else grpc.aio.insecure_channel(addr)
        )
        self._stub = kernel_service_pb2_grpc.KernelExecutionServiceStub(self._channel)

    @classmethod
    def from_env(cls) -> Optional["KernelClient"]:
        """Returns a client when ABOS_KERNEL_ADDR is set, otherwise None."""
        addr = os.getenv("ABOS_KERNEL_ADDR", "")
        if not addr:
            return None
        creds = None
        ca = os.getenv("ABOS_TLS_CA")
        if ca:
            def read(path):
                with open(path, "rb") as f:
                    return f.read()
            cert, key = os.getenv("ABOS_TLS_CERT"), os.getenv("ABOS_TLS_KEY")
            creds = grpc.ssl_channel_credentials(
                root_certificates=read(ca),
                private_key=read(key) if key else None,
                certificate_chain=read(cert) if cert else None,
            )
        return cls(addr, secret=os.getenv("ABOS_INTERNAL_SECRET", ""), credentials=creds)

    def _metadata(self):
        return (("authorization", f"Bearer {make_token(self._secret)}"),) if self._secret else ()

    async def create_ticket(
        self,
        workspace_id: str,
        idempotency_key: str,
        action_type: str,
        payload_json: str,
        agent_id: str = "",
        priority: int = 5,
    ) -> Dict[str, Any]:
        req = kernel_service_pb2.CreateTicketRequest(
            workspace_id=workspace_id,
            agent_id=agent_id,
            idempotency_key=idempotency_key,
            action_type=action_type,
            payload_json=payload_json,
            priority=priority,
        )
        try:
            resp = await self._stub.CreateTicket(req, metadata=self._metadata(), timeout=self._timeout)
        except grpc.aio.AioRpcError as e:
            raise KernelUnavailable(f"kernel CreateTicket failed: {e.code().name}") from e
        return {"ticket_id": resp.ticket_id, "status": resp.status, "error_message": resp.error_message}

    async def close(self) -> None:
        await self._channel.close()
