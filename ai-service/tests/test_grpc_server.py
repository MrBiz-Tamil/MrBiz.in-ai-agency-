"""In-process gRPC tests: real server, real stubs, HMAC auth."""
import grpc
import pytest

from config import DEV_WORKSPACE_ID
from gen.abos.agent.v1 import agent_service_pb2, agent_service_pb2_grpc
from grpc_server.server import serve
from router.llm_router import LLMRouter
from security.grpc_auth import make_token

SECRET = "t" * 40


async def _start(secret=SECRET):
    import grpc.aio as aio
    from grpc_server import server as srv

    interceptors = [srv.HMACAuthInterceptor(secret)] if secret else []
    s = aio.server(interceptors=interceptors)
    agent_service_pb2_grpc.add_AgentOrchestratorServiceServicer_to_server(srv.AgentOrchestratorServicer(LLMRouter()), s)
    port = s.add_insecure_port("127.0.0.1:0")
    await s.start()
    return s, port


def _metadata(secret=SECRET):
    return (("authorization", f"Bearer {make_token(secret)}"),)


async def test_unauthenticated_calls_are_rejected():
    s, port = await _start()
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{port}") as ch:
            stub = agent_service_pb2_grpc.AgentOrchestratorServiceStub(ch)
            req = agent_service_pb2.InspectPromptDLPRequest(workspace_id=DEV_WORKSPACE_ID, untrusted_content="hi", origin_system="web")
            with pytest.raises(grpc.aio.AioRpcError) as e:
                await stub.InspectPromptDLP(req)
            assert e.value.code() == grpc.StatusCode.UNAUTHENTICATED
            with pytest.raises(grpc.aio.AioRpcError) as e:
                await stub.InspectPromptDLP(req, metadata=_metadata("wrong-secret" * 4))
            assert e.value.code() == grpc.StatusCode.UNAUTHENTICATED
    finally:
        await s.stop(None)


async def test_authenticated_dlp_and_compile_work():
    s, port = await _start()
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{port}") as ch:
            stub = agent_service_pb2_grpc.AgentOrchestratorServiceStub(ch)
            dlp = await stub.InspectPromptDLP(
                agent_service_pb2.InspectPromptDLPRequest(
                    workspace_id=DEV_WORKSPACE_ID, untrusted_content="card 4532 0151 1283 0366", origin_system="shopify"
                ),
                metadata=_metadata(),
            )
            assert "CREDIT_CARD" in list(dlp.detected_pii_types)

            pol = await stub.CompilePolicy(
                agent_service_pb2.CompilePolicyRequest(workspace_id=DEV_WORKSPACE_ID, raw_policy_text="Pause any refund above 1500 INR"),
                metadata=_metadata(),
            )
            assert pol.trigger_event == "order.refund_requested"

            with pytest.raises(grpc.aio.AioRpcError) as e:
                await stub.CompilePolicy(
                    agent_service_pb2.CompilePolicyRequest(workspace_id=DEV_WORKSPACE_ID, raw_policy_text="refund above $5"),
                    metadata=_metadata(),
                )
            assert e.value.code() == grpc.StatusCode.INVALID_ARGUMENT

            with pytest.raises(grpc.aio.AioRpcError) as e:
                await stub.InspectPromptDLP(
                    agent_service_pb2.InspectPromptDLPRequest(workspace_id="ws_test_99", untrusted_content="x"),
                    metadata=_metadata(),
                )
            assert e.value.code() == grpc.StatusCode.INVALID_ARGUMENT
    finally:
        await s.stop(None)


async def test_canned_endpoints_unimplemented_in_production(monkeypatch):
    monkeypatch.setenv("ABOS_ENV", "production")
    s, port = await _start()
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{port}") as ch:
            stub = agent_service_pb2_grpc.AgentOrchestratorServiceStub(ch)
            with pytest.raises(grpc.aio.AioRpcError) as e:
                await stub.DecomposeGoal(
                    agent_service_pb2.DecomposeGoalRequest(workspace_id=DEV_WORKSPACE_ID, goal_description="x"),
                    metadata=_metadata(),
                )
            assert e.value.code() == grpc.StatusCode.UNIMPLEMENTED
    finally:
        await s.stop(None)


async def test_serve_requires_secret_in_production(monkeypatch):
    monkeypatch.setenv("ABOS_ENV", "production")
    monkeypatch.delenv("ABOS_INTERNAL_SECRET", raising=False)
    with pytest.raises(RuntimeError):
        await serve(port=0)
    with pytest.raises(RuntimeError):
        await serve(port=0, secret="short")
