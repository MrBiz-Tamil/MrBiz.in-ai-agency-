import asyncio
import os
import sys
import uuid
from pathlib import Path
from typing import Optional

import grpc
from loguru import logger

# Allow running as a script: `python grpc_server/server.py`
_ai_service_dir = Path(__file__).resolve().parent.parent
for _p in (str(_ai_service_dir), str(_ai_service_dir / "gen")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from compiler.policy_compiler import PolicyCompileError, PolicyCompiler  # noqa: E402
from config import is_dev  # noqa: E402
from gen.abos.agent.v1 import agent_service_pb2, agent_service_pb2_grpc  # noqa: E402
from gen.abos.voice.v1 import voice_service_pb2_grpc  # noqa: E402
from router.llm_router import LLMRouter, LLMUnavailableError  # noqa: E402
from security.dlp_shield import DLPShield  # noqa: E402
from security.grpc_auth import HMACAuthInterceptor  # noqa: E402
from voice.voice_orchestrator import VoiceOrchestratorServicer  # noqa: E402

MAX_DLP_INPUT_CHARS = 100_000


async def _require_workspace(workspace_id: str, context: grpc.aio.ServicerContext) -> None:
    try:
        uuid.UUID(workspace_id)
    except (ValueError, AttributeError, TypeError):
        await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "workspace_id must be a UUID")


async def _dev_only(feature: str, context: grpc.aio.ServicerContext) -> None:
    """Canned/simulated endpoints must never answer in production."""
    if not is_dev():
        await context.abort(grpc.StatusCode.UNIMPLEMENTED, f"{feature} is not implemented")


class AgentOrchestratorServicer(agent_service_pb2_grpc.AgentOrchestratorServiceServicer):
    """AgentOrchestratorService endpoints.

    CompilePolicy and InspectPromptDLP are real. DecomposeGoal, StreamReasoning and
    RunAgentHeartbeat still return canned data and are therefore available in dev only.
    """

    def __init__(self, router: Optional[LLMRouter] = None):
        self.router = router or LLMRouter()
        self.compiler = PolicyCompiler(self.router)
        self.dlp = DLPShield()

    async def DecomposeGoal(self, request, context):
        await _require_workspace(request.workspace_id, context)
        await _dev_only("DecomposeGoal", context)
        logger.info(f"[dev] Decomposing goal for workspace {request.workspace_id}")

        milestones = [
            agent_service_pb2.MilestonePlan(
                step_order=1,
                assigned_agent_role="Multi-Channel Catalog & Inventory Router",
                tool_action="inventory.sync_buffer",
                cron_schedule="0 */4 * * *",
                expected_outcome="Reconcile stock buffers between Shopify and Odoo",
            ),
            agent_service_pb2.MilestonePlan(
                step_order=2,
                assigned_agent_role="Sales & Cart Recovery Voice Agent",
                tool_action="telephony.dial_cart_recovery",
                cron_schedule="",
                expected_outcome="Callbacks to recover high-value checkout carts",
            ),
            agent_service_pb2.MilestonePlan(
                step_order=3,
                assigned_agent_role="Finance & Reconciliation Auditor",
                tool_action="books.match_payout",
                cron_schedule="0 23 * * *",
                expected_outcome="Settlement matching and draft invoice audit",
            ),
        ]
        return agent_service_pb2.DecomposeGoalResponse(
            plan_id=f"plan_{request.goal_id or 'auto_01'}",
            projected_margin_pct=26.4,
            estimated_days=21,
            milestones=milestones,
        )

    async def StreamReasoning(self, request, context):
        await _require_workspace(request.workspace_id, context)
        await _dev_only("StreamReasoning", context)
        steps = [
            ("Inspecting order SKUs and buffer limits in the hot cache...", "inventory.get_stock", '{"sku": "SKU-992"}', False),
            ("Detecting stock variance between Odoo and Shopify...", "inventory.reserve_stock", '{"sku": "SKU-992", "variance": 8}', False),
            ("Variance value exceeds threshold; holding for human approval.", "approval.request", '{"threshold_inr": 1000}', True),
        ]
        for idx, (thought, tool, input_json, needs_approval) in enumerate(steps, start=1):
            await asyncio.sleep(0.05)
            yield agent_service_pb2.StreamReasoningResponse(
                step_number=idx,
                thought=thought,
                proposed_tool=tool,
                tool_input_json=input_json,
                requires_human_approval=needs_approval,
            )

    async def CompilePolicy(self, request, context):
        await _require_workspace(request.workspace_id, context)
        try:
            res = await self.compiler.compile(request.raw_policy_text, request.workspace_id)
        except PolicyCompileError as e:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(e))
        except LLMUnavailableError:
            await context.abort(grpc.StatusCode.UNAVAILABLE, "no LLM provider available")
        return agent_service_pb2.CompilePolicyResponse(
            rule_id=res["rule_id"],
            trigger_event=res["trigger_event"],
            compiled_ast_json=res["compiled_ast_json"],
            assigned_approval_role=res["assigned_approval_role"],
        )

    async def InspectPromptDLP(self, request, context):
        await _require_workspace(request.workspace_id, context)
        if len(request.untrusted_content) > MAX_DLP_INPUT_CHARS:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "content too large")
        # Regex work is CPU-bound; keep it off the event loop.
        result = await asyncio.to_thread(self.dlp.inspect_and_sanitize, request.untrusted_content, request.origin_system)
        return agent_service_pb2.InspectPromptDLPResponse(
            is_safe=result["is_safe"],
            sanitized_content=result["sanitized_content"],
            detected_pii_types=result["detected_pii_types"],
            injection_detected=result["injection_detected"],
        )

    async def RunAgentHeartbeat(self, request, context):
        await _require_workspace(request.workspace_id, context)
        await _dev_only("RunAgentHeartbeat", context)
        return agent_service_pb2.RunAgentHeartbeatResponse(
            status="HEALTHY",
            tickets_spawned=0,
            log_summary=f"[dev] simulated heartbeat for {request.agent_role}",
        )


def _server_credentials() -> Optional[grpc.ServerCredentials]:
    cert, key, ca = os.getenv("ABOS_TLS_CERT"), os.getenv("ABOS_TLS_KEY"), os.getenv("ABOS_TLS_CA")
    if not cert:
        return None
    with open(cert, "rb") as f:
        chain = f.read()
    with open(key, "rb") as f:
        priv = f.read()
    root = None
    if ca:
        with open(ca, "rb") as f:
            root = f.read()
    return grpc.ssl_server_credentials([(priv, chain)], root_certificates=root, require_client_auth=bool(root))


async def serve(
    host: str = "127.0.0.1",
    port: int = 50051,
    secret: Optional[str] = None,
    router: Optional[LLMRouter] = None,
) -> grpc.aio.Server:
    secret = secret if secret is not None else os.getenv("ABOS_INTERNAL_SECRET", "")
    if not secret and not is_dev():
        raise RuntimeError("ABOS_INTERNAL_SECRET must be set outside development")
    if secret and not is_dev() and len(secret) < 32:
        raise RuntimeError("ABOS_INTERNAL_SECRET must be at least 32 characters")
    if not secret:
        logger.warning("ABOS_INTERNAL_SECRET not set: gRPC authentication is DISABLED (dev only)")

    interceptors = [HMACAuthInterceptor(secret)] if secret else []
    server = grpc.aio.server(interceptors=interceptors, options=[("grpc.max_receive_message_length", 4 * 1024 * 1024)])
    agent_service_pb2_grpc.add_AgentOrchestratorServiceServicer_to_server(AgentOrchestratorServicer(router), server)
    voice_service_pb2_grpc.add_VoiceOrchestratorServiceServicer_to_server(VoiceOrchestratorServicer(), server)

    listen_addr = f"{host}:{port}"
    creds = _server_credentials()
    bound = server.add_secure_port(listen_addr, creds) if creds else server.add_insecure_port(listen_addr)
    if bound == 0:
        raise RuntimeError(f"failed to bind {listen_addr}")
    logger.info(f"AI & Voice gRPC server listening on {listen_addr} (tls={'on' if creds else 'off'}, auth={'on' if secret else 'off'})")
    await server.start()
    return server


if __name__ == "__main__":
    async def _main():
        server = await serve(host=os.getenv("ABOS_AI_HOST", "127.0.0.1"), port=int(os.getenv("ABOS_AI_PORT", "50051")))
        await server.wait_for_termination()

    asyncio.run(_main())
