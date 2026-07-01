"""Log <-> trace correlation for the obs package.

A logging filter that stamps every record emitted while a span is active with that span's
``trace_id`` and ``span_id``. This lets a backend pivot from a log line to the trace it
belongs to (and, on the GCP path, Cloud Logging auto-links the two). It reads the active span
from the current context, so it is zero-cost when no span is active or tracing is off — the
record's ids are then the empty string, and a formatter that references them never fails.
"""

from __future__ import annotations

import logging

from opentelemetry import trace


class TraceCorrelationFilter(logging.Filter):
    """A logging filter that adds ``trace_id`` / ``span_id`` to each record from the active
    span. When no span is active — or tracing is disabled, in which case the active span is
    the framework's no-op span with an invalid context — both are set to the empty string, so
    a format string that includes them is always safe. It never drops a record (``filter``
    always returns True)."""

    def filter(self, record: logging.LogRecord) -> bool:
        sc = trace.get_current_span().get_span_context()
        if sc.is_valid:
            record.trace_id = trace.format_trace_id(sc.trace_id)
            record.span_id = trace.format_span_id(sc.span_id)
        else:
            record.trace_id = ""
            record.span_id = ""
        return True


def install_log_correlation(logger: logging.Logger | None = None) -> TraceCorrelationFilter:
    """Attach a :class:`TraceCorrelationFilter` to ``logger``'s handlers (the root logger by
    default) so records emitted under a span carry ``trace_id`` / ``span_id``. The entrypoint
    calls this once after configuring logging; correlation then applies to any log call made
    while a span is active. Returns the installed filter."""
    target = logger if logger is not None else logging.getLogger()
    filt = TraceCorrelationFilter()
    for handler in target.handlers:
        handler.addFilter(filt)
    return filt
