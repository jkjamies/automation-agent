"""obs — observability tooling: turn on the distributed tracing the agent framework already
emits but discards.

The framework builds a native span tree (``invoke_agent`` -> ``call_llm`` -> ``execute_tool``,
under the GenAI semantic conventions) for every run, but the spans go nowhere until a process
registers a tracer provider + exporter once at startup. This package is that registration,
plus the propagation and flush plumbing that stitches the trace across the Cloud Tasks
boundary. Off by default (``OTEL_TRACES_EXPORTER=none``). Deterministic tooling — no agent
imports; only ``config`` reads ``OTEL_*``. See ``.agents/standards/observability.md``.
"""

from automation_agent.obs.exporters import new_exporter, parse_otlp_headers
from automation_agent.obs.log import TraceCorrelationFilter, install_log_correlation
from automation_agent.obs.middleware import HEALTH_PATH, TracingMiddleware
from automation_agent.obs.obs import (
    DEFAULT_SERVICE_VERSION,
    EXPORTER_CONSOLE,
    EXPORTER_GCP,
    EXPORTER_NONE,
    EXPORTER_OTLP,
    FLUSH_TIMEOUT_MS,
    Config,
    Shutdown,
    flush,
    init,
    parse_sampler,
)
from automation_agent.obs.propagation import extract, inject

__all__ = [
    "DEFAULT_SERVICE_VERSION",
    "EXPORTER_CONSOLE",
    "EXPORTER_GCP",
    "EXPORTER_NONE",
    "EXPORTER_OTLP",
    "FLUSH_TIMEOUT_MS",
    "HEALTH_PATH",
    "Config",
    "Shutdown",
    "TraceCorrelationFilter",
    "TracingMiddleware",
    "extract",
    "flush",
    "init",
    "inject",
    "install_log_correlation",
    "new_exporter",
    "parse_otlp_headers",
    "parse_sampler",
]
