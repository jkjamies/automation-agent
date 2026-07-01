"""The tracer-provider registration at the heart of the obs package.

It builds and globally registers an OpenTelemetry tracer provider so the agent framework's
native span tree (the ``invoke_agent`` -> ``call_llm`` -> ``execute_tool`` tree it already
emits under the GenAI semantic conventions) is exported instead of discarded. We own the
provider; the agent framework inherits it via the OTel global — its tracer is a lazy proxy
that resolves the global provider on first use, so registering ours first is all it takes,
and this package never calls the framework's own telemetry setup. Everything is off by
default: with the exporter set to ``none`` (the default) :func:`init` is a no-op and nothing
about the running service changes.

Deterministic tooling — it imports no agent packages, and only ``config`` reads the ``OTEL_*``
environment (this package takes a typed :class:`Config`). See
``.agents/standards/observability.md``.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass

from opentelemetry import trace
from opentelemetry.propagate import set_global_textmap
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanExporter
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF, ALWAYS_ON, ParentBased, Sampler
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from automation_agent.obs.exporters import new_exporter

# The application speaks exactly these four sinks; it never names a vendor. Any OTLP-native
# backend is reached with EXPORTER_OTLP plus an endpoint (and optional headers), so switching
# vendors is a config change, not a code change.
EXPORTER_NONE = "none"  # no-op default: init installs nothing and the service is unchanged.
EXPORTER_CONSOLE = "console"  # spans to stdout — local dev and the playground.
EXPORTER_OTLP = "otlp"  # OTLP/HTTP to an endpoint (any OTLP backend or a local Collector).
EXPORTER_GCP = "gcp"  # Google Cloud Trace via Application Default Credentials.

# DEFAULT_SERVICE_VERSION labels spans when Config.service_version is unset.
DEFAULT_SERVICE_VERSION = "dev"

# FLUSH_TIMEOUT_MS bounds a force-flush. It is only a backstop for a pathologically wedged
# exporter, so it sits above the exporters' own request timeouts (OTLP / Cloud Trace default
# to ~10s): a tighter bound could cancel a slow-but-working export and lose the very trailing
# batch the flush exists to guarantee.
FLUSH_TIMEOUT_MS = 30_000

# Shutdown flushes and releases the tracer provider. It is always safe to call (a no-op when
# tracing is disabled) and is returned from init for a deferred call at process exit.
Shutdown = Callable[[], None]


@dataclass
class Config:
    """The typed observability configuration. ``config`` reads the ``OTEL_*`` environment
    into these fields; this package never touches the environment, so the "only config reads
    env" boundary holds."""

    # exporter is one of the EXPORTER_* constants. Empty is treated as EXPORTER_NONE.
    exporter: str = EXPORTER_NONE
    # service_name is the resource service.name attribute on every span.
    service_name: str = "automation-agent"
    # service_version is the resource service.version attribute; empty uses "dev".
    service_version: str = ""
    # otlp_endpoint is the OTLP/HTTP target URL (EXPORTER_OTLP only). config rejects an empty
    # endpoint for that exporter, so by the time init runs it is set.
    otlp_endpoint: str = ""
    # otlp_headers is the standard OTEL_EXPORTER_OTLP_HEADERS value: comma-separated key=value
    # pairs, used as OTLP request headers (EXPORTER_OTLP only).
    otlp_headers: str = ""
    # sampler is a standard OTEL_TRACES_SAMPLER value (e.g. parentbased_always_on).
    sampler: str = "parentbased_always_on"


def _noop_shutdown() -> None:
    """The disabled-tracing Shutdown: nothing to flush or release."""


def init(cfg: Config) -> Shutdown:
    """Build the tracer provider for ``cfg``, register it as the OTel global, and set the
    global W3C TraceContext propagator. The agent framework then attaches its native spans to
    our provider. With EXPORTER_NONE (the default) it installs nothing and returns a no-op
    Shutdown, leaving the process exactly as it was. The returned Shutdown should be called in
    the entrypoint's shutdown path; it force-flushes buffered spans before releasing the
    provider (the scale-to-zero span-loss guard, mirrored by :func:`flush` on the request
    path).

    Raises:
        ValueError: if ``cfg.exporter`` is not one of none|console|otlp|gcp.
    """
    name = cfg.exporter or EXPORTER_NONE
    if name == EXPORTER_NONE:
        return _noop_shutdown
    if name not in (EXPORTER_CONSOLE, EXPORTER_OTLP, EXPORTER_GCP):
        raise ValueError(
            f"obs: unknown OTEL_TRACES_EXPORTER {name!r} (want none|console|otlp|gcp)"
        )
    return _install(new_exporter(cfg), cfg)


def _install(exporter: SpanExporter, cfg: Config) -> Shutdown:
    """Build our SDK tracer provider over ``exporter``, set it as the OTel global, and
    register the W3C propagator. This is the shared tail of :func:`init` and the test seam
    that injects a recording exporter. The provider uses a BatchSpanProcessor (async export,
    efficient for the many spans an agent run emits); :func:`flush` forces it out in-request.
    """
    version = cfg.service_version or DEFAULT_SERVICE_VERSION
    provider = TracerProvider(
        resource=_resource_for(cfg.service_name, version),
        sampler=parse_sampler(cfg.sampler),
    )
    provider.add_span_processor(BatchSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    set_global_textmap(TraceContextTextMapPropagator())

    def shutdown() -> None:
        # provider.shutdown() force-flushes buffered spans, then releases the processor.
        provider.shutdown()

    return shutdown


def _resource_for(service_name: str, version: str) -> Resource:
    """Build the resource describing this service. The two attributes are the stable
    service.name / service.version keys every backend understands."""
    return Resource.create(
        {"service.name": service_name, "service.version": version}
    )


def parse_sampler(name: str) -> Sampler:
    """Map a standard OTEL_TRACES_SAMPLER value to a Sampler. The default
    parentbased_always_on records every locally-started trace and honors an upstream sampling
    decision — correct here because trace volume is one-per-webhook (the cost is
    spans-per-trace, not trace rate). An unrecognized value falls back to the default rather
    than failing: the sampler is advisory, not a correctness gate."""
    match name.strip():
        case "always_on":
            return ALWAYS_ON
        case "always_off":
            return ALWAYS_OFF
        case "parentbased_always_off":
            return ParentBased(ALWAYS_OFF)
        case _:
            return ParentBased(ALWAYS_ON)


def flush(timeout_millis: int = FLUSH_TIMEOUT_MS) -> None:
    """Force any buffered spans out through the exporter now. The HTTP middleware calls it
    before every traced response returns: the BatchSpanProcessor exports on a background
    timer, but Cloud Run throttles CPU the instant a response is sent, so an un-flushed
    trailing batch would be lost on scale-to-zero. It resolves the active global provider and
    is a no-op when tracing is disabled (the global is the framework's no-op proxy provider,
    which has no force_flush)."""
    provider = trace.get_tracer_provider()
    force = getattr(provider, "force_flush", None)
    if callable(force):
        force(timeout_millis)
