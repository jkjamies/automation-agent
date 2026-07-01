"""Shared test fixtures.

A ``FakeLlm`` (a scripted BaseLlm) yields scripted text and
records the requests it received, so we can test agent wiring and deterministic logic
without a real model. We never assert on real LLM output content.
"""

from __future__ import annotations

import opentelemetry.trace as _trace_api
import pytest
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.genai import types
from opentelemetry import propagate
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.util._once import Once
from pydantic import PrivateAttr


def _reset_otel_globals() -> None:
    # The OTel API sets its tracer provider exactly once per process; reset the set-once guard
    # so a test can register a fresh provider (mirrors the isolation the reference tests get by
    # snapshotting and restoring the globals).
    _trace_api._TRACER_PROVIDER_SET_ONCE = Once()
    _trace_api._TRACER_PROVIDER = None


@pytest.fixture
def reset_otel():
    """Snapshot the global tracer provider + propagator, clear them for the test, and restore
    them afterwards. obs.init / _install mutate process-global state; this keeps tests
    isolated (and leaves the rest of the suite on the default no-op proxy provider)."""
    saved_provider = _trace_api._TRACER_PROVIDER
    saved_once = _trace_api._TRACER_PROVIDER_SET_ONCE
    saved_propagator = propagate.get_global_textmap()
    _reset_otel_globals()
    try:
        yield
    finally:
        _trace_api._TRACER_PROVIDER = saved_provider
        _trace_api._TRACER_PROVIDER_SET_ONCE = saved_once
        propagate.set_global_textmap(saved_propagator)


@pytest.fixture
def otel_recording(reset_otel):
    """Register a provider exporting to a fresh in-memory exporter (via the same _install path
    init uses) and yield the exporter. The provider uses a BatchSpanProcessor, so ended spans
    are buffered until flush — exactly the production shape the flush-on-return guard depends
    on."""
    from automation_agent.obs.obs import Config, _install

    exporter = InMemorySpanExporter()
    shutdown = _install(exporter, Config(service_name="automation-agent-test"))
    try:
        yield exporter
    finally:
        shutdown()


class FakeLlm(BaseLlm):
    """A deterministic BaseLlm that yields fixed text responses in order."""

    _texts: list[str] = PrivateAttr(default_factory=list)
    _idx: int = PrivateAttr(default=0)
    _requests: list[LlmRequest] = PrivateAttr(default_factory=list)

    def __init__(self, *texts: str) -> None:
        super().__init__(model="fake")
        self._texts = list(texts) or [""]
        self._idx = 0
        self._requests = []

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False):  # type: ignore[override]
        self._requests.append(llm_request)
        text = self._texts[min(self._idx, len(self._texts) - 1)]
        self._idx += 1
        yield LlmResponse(
            content=types.Content(role="model", parts=[types.Part.from_text(text=text)]),
            turn_complete=True,
        )

    @property
    def requests(self) -> list[LlmRequest]:
        return self._requests


@pytest.fixture
def fake_llm():
    """Factory: ``fake_llm("a", "b")`` -> a FakeLlm yielding "a" then "b"."""

    def _make(*texts: str) -> FakeLlm:
        return FakeLlm(*texts)

    return _make
