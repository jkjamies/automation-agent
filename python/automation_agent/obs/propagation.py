"""Backend-aware trace-context propagation for the obs package.

The trace must cross the enqueue -> dispatch hop so the workflow trace continues from the
ingress span. This module is the ``Inject`` / ``Extract`` seam that abstracts both transport
backends: the Cloud Tasks backend injects the context as a W3C ``traceparent`` header on the
task, and the in-process backend inherits the context directly (the background dispatch task
copies the active execution context, so the span rides along with no carrier — mirroring how
it already skips the envelope JSON codec).
"""

from __future__ import annotations

from collections.abc import Mapping

from opentelemetry import propagate
from opentelemetry.context import Context


def inject(
    carrier: dict[str, str] | None = None, *, context: Context | None = None
) -> dict[str, str]:
    """Return the trace-context carrier (the W3C ``traceparent`` header, and ``tracestate``
    when present) for the active context, suitable for attaching to an outbound HTTP request.
    The Cloud Tasks transport merges it into the task's headers so the dispatch that runs the
    task continues the ingress trace. ``context`` defaults to the current context. When
    tracing is disabled — or the context carries no valid span — the propagator injects
    nothing, so the returned map is empty and no header is added to the task."""
    out = {} if carrier is None else carrier
    propagate.inject(out, context=context)
    return out


def extract(carrier: Mapping[str, str], *, context: Context | None = None) -> Context:
    """Return a context carrying the trace context found in ``carrier``, rooting a new span
    as a child of the upstream trace. The HTTP middleware extracts automatically from inbound
    request headers; this explicit helper backs the propagation round-trip tests and any
    non-HTTP carrier. A carrier with no trace context yields a context with no span."""
    return propagate.extract(carrier, context=context)
