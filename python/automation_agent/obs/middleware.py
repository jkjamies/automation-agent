"""The ASGI tracing middleware for the obs package.

Wraps the ASGI application so every inbound request (except the health probe) gets a server
span, and the span buffer is force-flushed before the response returns.

The server span is the trace root on the ingress path and continues the ingress trace on the
Cloud Tasks dispatch path: it is started from the W3C trace context read out of the inbound
headers via the global propagator, so a task carrying a ``traceparent`` header (injected by
the transport on enqueue) makes the dispatch span a child of the ingress span automatically.

The flush is load-bearing, not a tuning knob: the BatchSpanProcessor exports asynchronously,
but Cloud Run throttles CPU the instant a response is sent, so an un-flushed trailing batch is
lost when the instance is reclaimed. Flushing uniformly here — including on the fast 202
ingress path — costs one export per request (negligible at webhook volume) and removes the
scale-to-zero span-loss path entirely. When tracing is disabled, both the span and the flush
are no-ops, so the wrapped app behaves identically.
"""

from __future__ import annotations

import asyncio
from typing import Any

from opentelemetry import context as context_api
from opentelemetry import trace
from opentelemetry.trace import SpanKind, Status, StatusCode

from automation_agent.obs.obs import FLUSH_TIMEOUT_MS, flush
from automation_agent.obs.propagation import extract

# HEALTH_PATH is the liveness endpoint excluded from tracing: it is polled constantly and
# carries no causal interest, so a span per probe would be pure noise — and flushing on it
# would ship other requests' buffered batches early on the hottest path.
HEALTH_PATH = "/healthz"

# The instrumentation-scope name for the server spans this middleware creates.
_TRACER_NAME = "automation_agent.obs"

# ASGI types are structural; alias them for readability without a Starlette dependency.
_Scope = dict[str, Any]
_Receive = Any
_Send = Any


class TracingMiddleware:
    """ASGI middleware adding a server span per request and flushing spans before the
    response returns. Wrap the app with it and hand the result to the server."""

    def __init__(self, app: Any) -> None:
        self._app = app

    async def __call__(self, scope: _Scope, receive: _Receive, send: _Send) -> None:
        # Non-HTTP scopes (lifespan / websocket) and the constantly-polled health probe get no
        # span and no flush: the probe carries no causal interest, and a ForceFlush on it would
        # be pure overhead on the hottest path (and would ship other requests' batches early).
        if scope.get("type") != "http" or scope.get("path") == HEALTH_PATH:
            await self._app(scope, receive, send)
            return

        headers = {
            k.decode("latin-1"): v.decode("latin-1") for k, v in scope.get("headers", [])
        }
        parent = extract(headers)
        method = scope.get("method", "")
        path = scope.get("path", "")
        tracer = trace.get_tracer(_TRACER_NAME)
        span = tracer.start_span(f"{method} {path}", context=parent, kind=SpanKind.SERVER)
        token = context_api.attach(trace.set_span_in_context(span, parent))

        async def _send(message: dict[str, Any]) -> None:
            # Record the response status on the span (a free, body-independent attribute).
            if message["type"] == "http.response.start":
                span.set_attribute("http.response.status_code", message["status"])
            await send(message)

        try:
            await self._app(scope, receive, _send)
        except Exception as exc:
            # Record the failure on the span so a trace shows which request errored and why,
            # then re-raise so the server's own error handling is unchanged.
            span.set_status(Status(StatusCode.ERROR, str(exc)))
            span.record_exception(exc)
            raise
        finally:
            span.end()
            context_api.detach(token)
            await _flush_after_request()


async def _flush_after_request() -> None:
    """Export buffered spans while CPU is still allocated for this request. The (blocking)
    force-flush runs in a worker thread so it neither blocks the event loop nor is cancelled
    if the request coroutine is cancelled by a client disconnect — the flush must complete to
    guard against scale-to-zero span loss. The force-flush's own timeout is the hang backstop
    (see FLUSH_TIMEOUT_MS), so no tighter deadline is imposed here. When tracing is disabled
    the global provider has no force_flush, so this skips the thread hop entirely — the
    request path stays a true no-op."""
    if not hasattr(trace.get_tracer_provider(), "force_flush"):
        return
    loop = asyncio.get_running_loop()
    await loop.run_in_executor(None, flush, FLUSH_TIMEOUT_MS)
