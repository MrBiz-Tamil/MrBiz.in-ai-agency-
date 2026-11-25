"""Internal service-to-service authentication for gRPC.

Token format (matches backend-core/pkg/grpc/security.go):
    "<unix-ts>.<hex(hmac_sha256(secret, ts))>"  sent as  authorization: Bearer <token>
"""
import hashlib
import hmac
import time
from typing import Optional

import grpc

MAX_SKEW_SECONDS = 300


def make_token(secret: str, now: Optional[float] = None) -> str:
    ts = str(int(now if now is not None else time.time()))
    sig = hmac.new(secret.encode(), ts.encode(), hashlib.sha256).hexdigest()
    return f"{ts}.{sig}"


def verify_token(secret: str, token: str, now: Optional[float] = None) -> bool:
    try:
        ts_str, sig = token.split(".", 1)
        ts = int(ts_str)
    except ValueError:
        return False
    current = now if now is not None else time.time()
    if abs(current - ts) > MAX_SKEW_SECONDS:
        return False
    expected = hmac.new(secret.encode(), ts_str.encode(), hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, sig)


class HMACAuthInterceptor(grpc.aio.ServerInterceptor):
    """Rejects any RPC without a valid internal HMAC token."""

    def __init__(self, secret: str):
        if not secret:
            raise ValueError("HMACAuthInterceptor requires a non-empty secret")
        self._secret = secret

        def _deny(request, context):
            context.abort(grpc.StatusCode.UNAUTHENTICATED, "invalid authorization")

        self._deny_handler = grpc.unary_unary_rpc_method_handler(_deny)

    async def intercept_service(self, continuation, handler_call_details):
        md = dict(handler_call_details.invocation_metadata or ())
        auth = md.get("authorization", "")
        token = auth[7:] if auth.startswith("Bearer ") else auth
        if not verify_token(self._secret, token):
            # An unauthenticated caller learns nothing about which methods exist.
            return self._deny_handler
        return await continuation(handler_call_details)
