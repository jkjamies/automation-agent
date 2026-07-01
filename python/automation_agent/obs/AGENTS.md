# obs

Observability tooling: it turns on the distributed tracing the agent framework already emits
but throws away. The framework builds a native span tree — `invoke_agent` → `call_llm` →
`execute_tool`, under the GenAI semantic conventions — for every run, but the spans go nowhere
until a process registers a tracer provider + exporter once at startup. This package is that
registration, plus the propagation and flush plumbing that stitches the trace across the Cloud
Tasks boundary. See `.agents/standards/observability.md` for the language-neutral design and
decisions.

## Off by default

With `OTEL_TRACES_EXPORTER=none` (the default) `init` installs nothing and returns a no-op
`Shutdown`; the process is unchanged. Merging this package is a no-op in production until an
environment opts in.

## We own the provider

`obs` builds and globally registers **one** tracer provider; the framework inherits it via the
OTel global (its tracer is a lazy proxy that resolves the global provider on first span). We do
**not** call the framework's own telemetry setup — registering ours first is the conflict-free
integration. `init(cfg)` builds the exporter, constructs an SDK tracer provider (resource +
`BatchSpanProcessor` + sampler), sets it as the OTel global, and sets the W3C `TraceContext`
propagator.

## Responsibilities

- **`init` / `Shutdown`** — build + register the provider; the returned `Shutdown` force-flushes
  and releases it at process exit.
- **`TracingMiddleware`** — an ASGI wrapper: a server span per request (the trace root on
  ingress, continued from the task's `traceparent` on `/internal/dispatch`), `/healthz`
  excluded. It **force-flushes before the response returns** — load-bearing on Cloud Run, where
  CPU is throttled the instant a response is sent, so a still-buffered batch would be lost on
  scale-to-zero. The blocking flush runs in a worker thread so it is neither on the event loop
  nor cancelled by a client disconnect.
- **`inject` / `extract`** — backend-aware propagation. The Cloud Tasks transport injects the
  trace context as a `traceparent` **header**; the in-process transport needs no carrier — its
  background dispatch task copies the active execution context, so the span rides along.
- **`flush`** — force-export buffered spans now; used by the middleware and `Shutdown`.
- **`install_log_correlation`** — attaches a filter so records emitted under an active span
  carry `trace_id`/`span_id` (log↔trace correlation).

## Exporters (`none | console | otlp | gcp`)

The application speaks exactly four sinks and **never names a vendor**. Any OTLP-native backend
(New Relic, Datadog, Honeycomb, Grafana, a local Collector, …) is `otlp` +
`OTEL_EXPORTER_OTLP_ENDPOINT` + `OTEL_EXPORTER_OTLP_HEADERS` — zero code. `gcp` is the one
convenience path: Cloud Trace via Application Default Credentials.

## Boundaries

Deterministic tooling — it imports **no** agent packages (arch-test enforced), and it reads
**no** environment: `config` is the only place that reads `OTEL_*`, and hands this package a
typed `Config`. Comments and code here name no other language stack.
