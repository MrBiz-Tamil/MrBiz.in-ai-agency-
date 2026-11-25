"""Reasoner -> Go kernel CreateTicket, against an in-process fake kernel (real gRPC + HMAC auth)."""
import grpc
import pytest

from config import DEV_WORKSPACE_ID
from gen.abos.kernel.v1 import kernel_service_pb2, kernel_service_pb2_grpc
from gen.abos.voice.v1 import voice_service_pb2
from security.grpc_auth import HMACAuthInterceptor
from voice.hot_cache import VoiceHotCache
from voice.kernel_client import KernelClient
from voice.reasoner import AsynchronousReasoner, MutationDispatchUnavailable, idempotency_key
from voice.voice_orchestrator import VoiceOrchestratorServicer

SECRET = "k" * 40


class FakeKernel(kernel_service_pb2_grpc.KernelExecutionServiceServicer):
    def __init__(self, status="PENDING_APPROVAL"):
        self.requests = []
        self.status = status

    async def CreateTicket(self, request, context):
        self.requests.append(request)
        return kernel_service_pb2.CreateTicketResponse(ticket_id="0b8f3d5e-2f7a-4c1b-8d9e-1a2b3c4d5e6f", status=self.status)


async def start(fake):
    server = grpc.aio.server(interceptors=[HMACAuthInterceptor(SECRET)])
    kernel_service_pb2_grpc.add_KernelExecutionServiceServicer_to_server(fake, server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    return server, f"127.0.0.1:{port}"


MUTATION = {"action": "order.update_shipping_address", "order_id": "ord_1042", "requested_change": "to Warehouse 4", "caller_phone": "+919800000001"}


async def test_reasoner_creates_a_real_ticket_with_auth_and_stable_key():
    fake = FakeKernel("PENDING_APPROVAL")
    server, addr = await start(fake)
    try:
        client = KernelClient(addr, secret=SECRET)
        reasoner = AsynchronousReasoner(kernel=client)
        res = await reasoner.dispatch_erp_mutation(DEV_WORKSPACE_ID, MUTATION, call_id="call-1")
        again = await reasoner.dispatch_erp_mutation(DEV_WORKSPACE_ID, MUTATION, call_id="call-1")
        await client.close()
    finally:
        await server.stop(None)

    assert res["status"] == "PENDING_APPROVAL" and res["simulated"] is False
    assert res["ticket_id"] == "0b8f3d5e-2f7a-4c1b-8d9e-1a2b3c4d5e6f"
    assert len(fake.requests) == 2
    r = fake.requests[0]
    assert r.workspace_id == DEV_WORKSPACE_ID and r.action_type == "order.update_shipping_address"
    assert r.idempotency_key == fake.requests[1].idempotency_key == idempotency_key(DEV_WORKSPACE_ID, "call-1", MUTATION)
    assert idempotency_key(DEV_WORKSPACE_ID, "call-2", MUTATION) != r.idempotency_key


async def test_wrong_secret_is_refused_and_reported_as_unavailable():
    server, addr = await start(FakeKernel())
    try:
        client = KernelClient(addr, secret="x" * 40)
        with pytest.raises(MutationDispatchUnavailable):
            await AsynchronousReasoner(kernel=client).dispatch_erp_mutation(DEV_WORKSPACE_ID, MUTATION, call_id="c")
        await client.close()
    finally:
        await server.stop(None)


async def test_failed_ticket_is_not_reported_as_accepted():
    server, addr = await start(FakeKernel("FAILED"))
    try:
        client = KernelClient(addr, secret=SECRET)
        with pytest.raises(MutationDispatchUnavailable):
            await AsynchronousReasoner(kernel=client).dispatch_erp_mutation(DEV_WORKSPACE_ID, MUTATION, call_id="c")
        await client.close()
    finally:
        await server.stop(None)


async def test_unreachable_kernel_raises_unavailable():
    client = KernelClient("127.0.0.1:1", secret=SECRET, timeout=0.5)
    with pytest.raises(MutationDispatchUnavailable):
        await AsynchronousReasoner(kernel=client).dispatch_erp_mutation(DEV_WORKSPACE_ID, MUTATION, call_id="c")
    await client.close()


async def test_voice_call_end_to_end_tells_the_truth():
    """The caller-facing status follows what the kernel actually did."""
    fake = FakeKernel("PENDING_APPROVAL")
    server, addr = await start(fake)
    try:
        client = KernelClient(addr, secret=SECRET)
        svc = VoiceOrchestratorServicer(cache=VoiceHotCache())
        svc.reasoner = AsynchronousReasoner(kernel=client)
        req = voice_service_pb2.DispatchVoiceMutationRequest(
            workspace_id=DEV_WORKSPACE_ID, call_id="call-9", caller_phone="+919800000001",
            payload_json="please change address for order ord_1042 to Warehouse 4",
        )
        resp = await svc.DispatchVoiceMutation(req, None)
        assert resp.status == "PENDING_APPROVAL" and resp.ticket_id and "recorded" in resp.conversational_ack.lower()
        assert len(fake.requests) == 1

        # Kernel down: the caller is told nothing was submitted.
        await server.stop(None)
        resp = await svc.DispatchVoiceMutation(req, None)
        assert resp.status == "UNAVAILABLE" and resp.ticket_id == "" and "wasn't able" in resp.conversational_ack
        await client.close()
    finally:
        await server.stop(None)
