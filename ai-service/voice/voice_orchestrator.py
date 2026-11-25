import asyncio
import json
from typing import AsyncIterator

import grpc
from loguru import logger

from config import is_dev
from gen.abos.voice.v1 import voice_service_pb2, voice_service_pb2_grpc
from voice.hot_cache import VoiceHotCache
from voice.reasoner import AsynchronousReasoner, MutationDispatchUnavailable
from voice.talker import SynchronousTalker


def _transcript_from_payload(payload_json: str) -> str:
    """payload_json is either {"transcript": "..."} or the raw transcript text."""
    try:
        data = json.loads(payload_json)
        if isinstance(data, dict) and isinstance(data.get("transcript"), str):
            return data["transcript"]
    except (json.JSONDecodeError, TypeError):
        pass
    return payload_json


class VoiceOrchestratorServicer(voice_service_pb2_grpc.VoiceOrchestratorServiceServicer):
    """gRPC implementation of VoiceOrchestratorService."""

    def __init__(self, cache: VoiceHotCache = None):
        self.cache = cache or VoiceHotCache()
        self.talker = SynchronousTalker(self.cache)
        self.reasoner = AsynchronousReasoner()

    async def StreamTalkerAudio(
        self, request: voice_service_pb2.StreamTalkerAudioRequest, context: grpc.aio.ServicerContext
    ) -> AsyncIterator[voice_service_pb2.StreamTalkerAudioResponse]:
        # No TTS/LiveKit pipeline exists yet; never emit fake audio outside development.
        if not is_dev():
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "audio streaming is not implemented")
        frames = [b"\x00\x01\x02\x03\x04\x05", b"\x06\x07\x08\x09\x0a\x0b", b"\x0c\x0d\x0e\x0f\x10\x11"]
        for seq, frame in enumerate(frames, start=1):
            await asyncio.sleep(0.04)
            yield voice_service_pb2.StreamTalkerAudioResponse(
                call_id=request.call_id,
                sequence_number=seq,
                audio_bytes=frame,
                encoding="pcm_16000",
                is_final=(seq == len(frames)),
            )

    async def DispatchVoiceMutation(
        self, request: voice_service_pb2.DispatchVoiceMutationRequest, context: grpc.aio.ServicerContext
    ) -> voice_service_pb2.DispatchVoiceMutationResponse:
        logger.info(f"DispatchVoiceMutation for call {request.call_id}")

        # The talker does blocking Redis I/O; keep it off the event loop.
        spoken_reply, needs_mutation, mutation_data = await asyncio.to_thread(
            self.talker.process_utterance,
            request.workspace_id,
            request.caller_phone,
            _transcript_from_payload(request.payload_json),
        )

        if not (needs_mutation and mutation_data):
            return voice_service_pb2.DispatchVoiceMutationResponse(
                ticket_id="", status="RESOLVED_CONVERSATIONALLY", conversational_ack=spoken_reply
            )

        try:
            res = await self.reasoner.dispatch_erp_mutation(
                request.workspace_id, mutation_data, call_id=request.call_id, key=request.idempotency_key
            )
        except MutationDispatchUnavailable:
            # Be truthful with the caller: nothing was submitted.
            return voice_service_pb2.DispatchVoiceMutationResponse(
                ticket_id="",
                status="UNAVAILABLE",
                conversational_ack=(
                    "I wasn't able to submit that change just now. "
                    "I'll connect you with a team member who can help."
                ),
            )
        return voice_service_pb2.DispatchVoiceMutationResponse(
            ticket_id=res["ticket_id"], status=res["status"], conversational_ack=spoken_reply
        )

    async def StreamCallTelemetry(
        self, request: voice_service_pb2.StreamCallTelemetryRequest, context: grpc.aio.ServicerContext
    ) -> AsyncIterator[voice_service_pb2.StreamCallTelemetryResponse]:
        if not is_dev():
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "call telemetry is not implemented")
        samples = [
            (2, "NEUTRAL", "Customer: Where is my order #1042?", False),
            (4, "POSITIVE", "Talker: Your order is out for delivery.", False),
            (6, "POSITIVE", "Customer: Thank you!", False),
        ]
        for elapsed, sentiment, transcript, escalate in samples:
            await asyncio.sleep(0.1)
            yield voice_service_pb2.StreamCallTelemetryResponse(
                call_id=request.call_id,
                elapsed_seconds=elapsed,
                sentiment=sentiment,
                transcript_delta=transcript,
                supervisor_escalation_flag=escalate,
            )
