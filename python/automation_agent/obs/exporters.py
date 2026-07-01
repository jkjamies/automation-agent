"""Span-exporter construction for the obs package.

Builds the one exporter selected by config. The application names no vendor: every OTLP
backend is reached through EXPORTER_OTLP + endpoint, and EXPORTER_GCP is the one convenience
path (Cloud Trace via Application Default Credentials).
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from opentelemetry.sdk.trace.export import ConsoleSpanExporter, SpanExporter

if TYPE_CHECKING:
    from automation_agent.obs.obs import Config


def new_exporter(cfg: Config) -> SpanExporter:
    """Build the span exporter for ``cfg.exporter``. The caller has already rejected
    EXPORTER_NONE (no exporter) and any unknown value, so this handles only the three real
    sinks.

    Raises:
        ValueError: on an EXPORTER_OTLP config with no endpoint, or an unknown exporter.
    """
    # Imported here (deferred), not at module load: the OTLP and Cloud Trace exporter packages
    # pull in heavy transitive deps (protobuf, the Cloud Trace client), and the default
    # no-exporter path must not pay that import cost.
    from automation_agent.obs.obs import EXPORTER_CONSOLE, EXPORTER_GCP, EXPORTER_OTLP

    match cfg.exporter:
        case s if s == EXPORTER_CONSOLE:
            return ConsoleSpanExporter()
        case s if s == EXPORTER_OTLP:
            if not cfg.otlp_endpoint.strip():
                # config validates this, but guard so a direct caller fails loudly rather than
                # silently exporting nowhere.
                raise ValueError(f"obs: exporter {EXPORTER_OTLP!r} requires an OTLP endpoint")
            from opentelemetry.exporter.otlp.proto.http.trace_exporter import (
                OTLPSpanExporter,
            )

            headers = parse_otlp_headers(cfg.otlp_headers)
            return OTLPSpanExporter(
                endpoint=cfg.otlp_endpoint,
                headers=headers if headers else None,
            )
        case s if s == EXPORTER_GCP:
            # No project id: the Cloud Trace exporter detects it from Application Default
            # Credentials / the metadata server, matching how the rest of the GCP path
            # authenticates.
            from opentelemetry.exporter.cloud_trace import CloudTraceSpanExporter

            return CloudTraceSpanExporter()
        case _:
            raise ValueError(f"obs: unknown OTEL_TRACES_EXPORTER {cfg.exporter!r}")


def parse_otlp_headers(raw: str) -> dict[str, str]:
    """Parse the standard OTEL_EXPORTER_OTLP_HEADERS form — comma-separated key=value pairs
    (e.g. ``"api-key=secret,env=prod"``) — into a header map. Blank entries and entries
    without a key are skipped; only the first ``=`` splits, so a value may contain ``=``."""
    out: dict[str, str] = {}
    for pair in raw.split(","):
        pair = pair.strip()
        if not pair:
            continue
        key, sep, value = pair.partition("=")
        key = key.strip()
        if not sep or not key:
            continue
        out[key] = value.strip()
    return out
