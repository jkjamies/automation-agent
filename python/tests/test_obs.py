"""Tests for the observability package (tracer registration, exporters, propagation,
middleware, log correlation).

Deterministic: no live network, no LLM. We assert on span names / attributes / structure,
never on model output. The ``reset_otel`` and ``otel_recording`` fixtures (in conftest) keep
the process-global tracer provider isolated per test.
"""

from __future__ import annotations

import logging
from typing import Any

import opentelemetry.trace as trace_api
import pytest
from opentelemetry import context as context_api
from opentelemetry import trace
from opentelemetry.propagate import get_global_textmap
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF, ALWAYS_ON, ParentBased
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from automation_agent import obs
from automation_agent.obs.exporters import parse_otlp_headers
from automation_agent.obs.obs import Config, parse_sampler

# --- helpers -----------------------------------------------------------------


def _emit_fake_agent_tree() -> None:
    """Create the agent framework's native span shape — invoke_agent -> call_llm ->
    execute_tool — with representative GenAI-semconv attribute keys, without any model call.
    It stands in for the framework's auto-emitted tree so tests assert on span structure and
    attribute keys (never on LLM output text)."""
    tracer = trace.get_tracer("obs-test")
    invoke = tracer.start_span("invoke_agent automation_agent")
    invoke_ctx = trace.set_span_in_context(invoke)
    llm = tracer.start_span("call_llm gemma", context=invoke_ctx)
    llm.set_attribute("gen_ai.operation.name", "chat")
    llm.set_attribute("gen_ai.request.model", "gemma4:12b")
    llm.set_attribute("gen_ai.usage.input_tokens", 12)
    llm_ctx = trace.set_span_in_context(llm, invoke_ctx)
    tool = tracer.start_span("execute_tool apply_fix", context=llm_ctx)
    tool.set_attribute("gen_ai.tool.name", "apply_fix")
    tool.end()
    llm.end()
    invoke.end()


def _has_attr_key(span: Any, key: str) -> bool:
    return key in (span.attributes or {})


# --- init / provider registration --------------------------------------------


def test_init_none_is_noop(reset_otel) -> None:
    before = trace.get_tracer_provider()
    shutdown = obs.init(Config(exporter=obs.EXPORTER_NONE))
    assert trace.get_tracer_provider() is before, "init(none) must install nothing"
    shutdown()  # the no-op Shutdown must not raise


def test_init_empty_exporter_is_noop(reset_otel) -> None:
    before = trace.get_tracer_provider()
    obs.init(Config(exporter=""))
    assert trace.get_tracer_provider() is before, "empty exporter must mean none"


def test_init_unknown_exporter_rejected(reset_otel) -> None:
    with pytest.raises(ValueError, match="unknown OTEL_TRACES_EXPORTER"):
        obs.init(Config(exporter="jaeger"))


def test_init_console_registers_provider(reset_otel) -> None:
    before = trace.get_tracer_provider()
    shutdown = obs.init(Config(exporter=obs.EXPORTER_CONSOLE, service_name="automation-agent"))
    try:
        assert trace.get_tracer_provider() is not before
        # The propagator must be the W3C TraceContext so cross-process propagation round-trips.
        assert isinstance(get_global_textmap(), TraceContextTextMapPropagator)
    finally:
        shutdown()


def test_init_otlp_requires_endpoint(reset_otel) -> None:
    with pytest.raises(ValueError, match="OTLP endpoint"):
        obs.init(Config(exporter=obs.EXPORTER_OTLP))


def test_init_otlp_with_endpoint_builds(reset_otel) -> None:
    before = trace.get_tracer_provider()
    # The OTLP/HTTP exporter does not dial at construction, so a well-formed endpoint builds a
    # provider without a live collector.
    shutdown = obs.init(
        Config(
            exporter=obs.EXPORTER_OTLP,
            service_name="automation-agent",
            otlp_endpoint="http://localhost:4318/v1/traces",
            otlp_headers="api-key=secret",
        )
    )
    try:
        assert trace.get_tracer_provider() is not before
    finally:
        shutdown()


def test_init_gcp_without_credentials(reset_otel) -> None:
    # The Cloud Trace exporter needs Application Default Credentials. In a unit environment
    # there may be none, so init can surface a build error; if ADC happens to be present it
    # builds. Accept both, exercising the branch.
    try:
        shutdown = obs.init(Config(exporter=obs.EXPORTER_GCP, service_name="automation-agent"))
    except Exception:  # noqa: BLE001 - a missing-ADC build error is an acceptable outcome
        return
    shutdown()


# --- span tree + flush -------------------------------------------------------


def test_recorded_span_tree_and_genai_attributes(otel_recording: InMemorySpanExporter) -> None:
    _emit_fake_agent_tree()
    obs.flush()

    spans = otel_recording.get_finished_spans()
    assert len(spans) == 3, "expected invoke_agent -> call_llm -> execute_tool"
    by_name = {s.name: s for s in spans}
    invoke = by_name["invoke_agent automation_agent"]
    llm = by_name["call_llm gemma"]
    tool = by_name["execute_tool apply_fix"]

    # Tree shape: call_llm is a child of invoke_agent; execute_tool a child of call_llm; all
    # three share one trace.
    assert llm.parent is not None and llm.parent.span_id == invoke.context.span_id
    assert tool.parent is not None and tool.parent.span_id == llm.context.span_id
    assert invoke.context.trace_id == llm.context.trace_id == tool.context.trace_id

    # GenAI-semconv attribute keys are preserved (assert on keys/structure, not LLM text).
    assert _has_attr_key(llm, "gen_ai.request.model")
    assert _has_attr_key(llm, "gen_ai.usage.input_tokens")
    assert _has_attr_key(tool, "gen_ai.tool.name")


def test_flush_exports_before_return(otel_recording: InMemorySpanExporter) -> None:
    _emit_fake_agent_tree()
    # The BatchSpanProcessor buffers ended spans and exports on a background timer. Without an
    # explicit flush nothing has shipped yet — this is the scale-to-zero loss window.
    assert len(otel_recording.get_finished_spans()) == 0
    obs.flush()
    assert len(otel_recording.get_finished_spans()) > 0, "flush did not export buffered spans"


def test_flush_without_provider_is_noop(reset_otel) -> None:
    # With tracing disabled the global is the framework's no-op proxy provider (no
    # force_flush); flush must be a safe no-op rather than raise.
    trace_api.set_tracer_provider(trace_api.NoOpTracerProvider())
    obs.flush()  # must not raise


# --- sampler / headers parsing ----------------------------------------------


def test_parse_sampler_defaults() -> None:
    # Unknown/empty fall back to the always-on default rather than failing.
    for name in ("", "parentbased_always_on", "nonsense"):
        assert parse_sampler(name).get_description() == ParentBased(ALWAYS_ON).get_description()
    assert parse_sampler("always_off").get_description() == ALWAYS_OFF.get_description()
    assert parse_sampler("always_on").get_description() == ALWAYS_ON.get_description()
    assert (
        parse_sampler("parentbased_always_off").get_description()
        == ParentBased(ALWAYS_OFF).get_description()
    )


def test_parse_otlp_headers() -> None:
    got = parse_otlp_headers("api-key=secret , env=prod,bad,=novalue,k=a=b")
    assert got == {"api-key": "secret", "env": "prod", "k": "a=b"}


def test_new_exporter_rejects_unknown() -> None:
    # init pre-validates the exporter, but new_exporter guards defensively so a direct caller
    # fails loudly rather than returning None.
    from automation_agent.obs.exporters import new_exporter

    with pytest.raises(ValueError, match="unknown OTEL_TRACES_EXPORTER"):
        new_exporter(Config(exporter="jaeger"))


# --- middleware --------------------------------------------------------------


async def _call_asgi(
    app: Any, method: str, path: str, headers: dict[str, str] | None = None
) -> int:
    """Drive an ASGI app through one request and return the response status code."""
    scope = {
        "type": "http",
        "method": method,
        "path": path,
        "headers": [(k.lower().encode(), v.encode()) for k, v in (headers or {}).items()],
    }
    sent: list[dict[str, Any]] = []

    async def receive() -> dict[str, Any]:
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message: dict[str, Any]) -> None:
        sent.append(message)

    await app(scope, receive, send)
    start = next(m for m in sent if m["type"] == "http.response.start")
    return int(start["status"])


def _accepting_app(status: int = 202) -> Any:
    async def app(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": status, "headers": []})
        await send({"type": "http.response.body", "body": b"", "more_body": False})

    return app


async def test_middleware_one_span_per_request(otel_recording: InMemorySpanExporter) -> None:
    calls = 0

    async def app(scope: Any, receive: Any, send: Any) -> None:
        nonlocal calls
        calls += 1
        await send({"type": "http.response.start", "status": 202, "headers": []})
        await send({"type": "http.response.body", "body": b"", "more_body": False})

    status = await _call_asgi(obs.TracingMiddleware(app), "POST", "/webhooks/lint")
    assert calls == 1
    assert status == 202, "middleware must not alter the response"
    # The middleware flushes after the app returns, so the server span is already exported.
    spans = otel_recording.get_finished_spans()
    assert len(spans) == 1
    assert spans[0].name == "POST /webhooks/lint"


async def test_middleware_excludes_health(otel_recording: InMemorySpanExporter) -> None:
    status = await _call_asgi(obs.TracingMiddleware(_accepting_app(200)), "GET", obs.HEALTH_PATH)
    assert status == 200
    assert len(otel_recording.get_finished_spans()) == 0, "health probe must be excluded"


async def test_middleware_health_does_not_flush(otel_recording: InMemorySpanExporter) -> None:
    # Buffer spans outside any request (BatchSpanProcessor holds them until a flush).
    _emit_fake_agent_tree()
    mw = obs.TracingMiddleware(_accepting_app(200))

    # The health probe must leave the buffered spans un-exported.
    await _call_asgi(mw, "GET", obs.HEALTH_PATH)
    assert len(otel_recording.get_finished_spans()) == 0, "health probe must skip the flush"

    # A traced request does flush them (its own server span plus the buffered ones).
    await _call_asgi(obs.TracingMiddleware(_accepting_app(202)), "POST", "/webhooks/lint")
    assert len(otel_recording.get_finished_spans()) > 0


async def test_middleware_records_exception(otel_recording: InMemorySpanExporter) -> None:
    # When the wrapped app raises, the exception propagates but the server span records it
    # (ERROR status + an exception event) and is still ended and flushed.
    async def app(scope: Any, receive: Any, send: Any) -> None:
        raise RuntimeError("boom")

    with pytest.raises(RuntimeError, match="boom"):
        await _call_asgi(obs.TracingMiddleware(app), "POST", "/webhooks/lint")

    obs.flush()
    span = next(s for s in otel_recording.get_finished_spans() if s.name == "POST /webhooks/lint")
    assert span.status.status_code.name == "ERROR"
    assert any(ev.name == "exception" for ev in span.events)


async def test_middleware_continues_incoming_trace(otel_recording: InMemorySpanExporter) -> None:
    # A task carrying a traceparent header makes the server span a child of the upstream trace.
    ingress_ctx, ingress = _start_span()
    carrier = obs.inject(context=ingress_ctx)
    status = await _call_asgi(
        obs.TracingMiddleware(_accepting_app(200)), "POST", "/internal/dispatch", carrier
    )
    assert status == 200
    obs.flush()
    span = next(
        s for s in otel_recording.get_finished_spans() if s.name == "POST /internal/dispatch"
    )
    assert span.context.trace_id == ingress.trace_id, "dispatch span did not continue the trace"


# --- propagation -------------------------------------------------------------


def _start_span(name: str = "ingress"):
    """Root a sampled span (the recording provider is parentbased-always-on) and return its
    context and span context. An unsampled span would inject no traceparent."""
    ctx, span = _start(name)
    return ctx, span.get_span_context()


def _start(name: str):
    span = trace.get_tracer("obs-test").start_span(name)
    return trace.set_span_in_context(span), span


def test_inject_extract_round_trip_cloudtasks(otel_recording: InMemorySpanExporter) -> None:
    ingress_ctx, ingress = _start_span()

    # Enqueue side: inject the trace context into the (would-be Cloud Tasks) headers.
    carrier = obs.inject(context=ingress_ctx)
    assert carrier.get("traceparent"), "inject produced no traceparent for a sampled span"

    # Dispatch side: a fresh request with only the carrier reconstructs the trace.
    dispatch_ctx = obs.extract(carrier)
    extracted = trace.get_current_span(dispatch_ctx).get_span_context()
    assert extracted.trace_id == ingress.trace_id
    assert extracted.is_remote, "extracted context should be marked remote (crossed a hop)"

    # The dispatch root span continues the ingress trace with a new span id.
    _, dispatch = _start_in(dispatch_ctx, "dispatch")
    assert dispatch.get_span_context().trace_id == ingress.trace_id
    assert dispatch.get_span_context().span_id != ingress.span_id


def _start_in(parent, name: str):
    span = trace.get_tracer("obs-test").start_span(name, context=parent)
    return trace.set_span_in_context(span, parent), span


def test_inprocess_passthrough_shares_trace(otel_recording: InMemorySpanExporter) -> None:
    # The in-process backend carries the span on the active context: a background task copies
    # the current execution context at creation, so the span — and thus the trace — rides
    # along with no carrier. Model that with an explicit context capture.
    ingress_ctx, ingress = _start_span()
    carried = trace.get_current_span(ingress_ctx).get_span_context()
    assert carried.trace_id == ingress.trace_id and carried.span_id == ingress.span_id

    # Both backends yield the same logical trace: the cloudtasks header round-trip and the
    # inprocess passthrough resolve to one trace id.
    via_header = trace.get_current_span(obs.extract(obs.inject(context=ingress_ctx)))
    assert via_header.get_span_context().trace_id == carried.trace_id


def test_inject_disabled_is_empty(reset_otel) -> None:
    # With no active span (tracing effectively off), inject adds nothing — no traceparent
    # leaks onto a task when the feature is disabled.
    assert obs.inject() == {}


# --- log correlation ---------------------------------------------------------


def _record_with(handler_filter, ctx=None) -> logging.LogRecord:
    rec = logging.LogRecord("t", logging.INFO, __file__, 1, "msg", None, None)
    token = context_api.attach(ctx) if ctx is not None else None
    try:
        handler_filter.filter(rec)
    finally:
        if token is not None:
            context_api.detach(token)
    return rec


def test_log_filter_adds_trace_context(otel_recording: InMemorySpanExporter) -> None:
    ctx, span = _start("work")
    sc = span.get_span_context()
    rec = _record_with(obs.TraceCorrelationFilter(), ctx)
    span.end()
    assert rec.trace_id == trace.format_trace_id(sc.trace_id)
    assert rec.span_id == trace.format_span_id(sc.span_id)


def test_log_filter_no_span_is_empty() -> None:
    rec = _record_with(obs.TraceCorrelationFilter())
    assert rec.trace_id == "" and rec.span_id == ""


def test_log_filter_invalid_span_is_empty() -> None:
    # A record under a no-op span (invalid context) gains no ids.
    ctx = trace.set_span_in_context(trace.INVALID_SPAN)
    rec = _record_with(obs.TraceCorrelationFilter(), ctx)
    assert rec.trace_id == "" and rec.span_id == ""


def test_install_log_correlation_surfaces_ids(otel_recording: InMemorySpanExporter) -> None:
    logger = logging.getLogger("obs-corr-test")
    logger.handlers = []
    logger.propagate = False
    logger.setLevel(logging.INFO)
    records: list[logging.LogRecord] = []

    class _Capture(logging.Handler):
        def emit(self, record: logging.LogRecord) -> None:
            records.append(record)

    logger.addHandler(_Capture())
    obs.install_log_correlation(logger)

    ctx, span = _start("work")
    token = context_api.attach(ctx)
    try:
        logger.info("under a span")
    finally:
        context_api.detach(token)
        span.end()

    assert records and records[0].trace_id == trace.format_trace_id(
        span.get_span_context().trace_id
    )
