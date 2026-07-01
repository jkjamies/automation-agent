# Observability (distributed tracing)

How the automation-agent is traced, stated language-neutrally. The **Go** port (`go/internal/obs`)
is the reference implementation; the Python, TypeScript, and Kotlin ports mirror this contract
with their native SDKs (parity is at the **data** level — same span names/attributes — not the
registration code). This document is the design record: the rationale and decisions live here.

> **Scope: traces only.** Metrics and a log-bridge signal are deferred. This document covers the
> trace pipeline: provider registration, exporters, propagation, the flush constraint, config, and
> log↔trace correlation.

---

## The core idea: the framework already traces; we only turn it on

Every port's agent framework (`adk-go`, `adk-python`, `adk-js`, `adk-kotlin`) **already emits** a
native span tree for every run, under the OpenTelemetry **GenAI semantic conventions**:

```
invoke_agent  ──▶  call_llm (gen_ai.request.model, token usage, finish reason)  ──▶  execute_tool (gen_ai.tool.name)
```

Those spans are created and then **discarded**, because nothing in the process stands up a tracer
provider or exporter. The observability package is that missing registration — plus the
propagation and flush plumbing to stitch the tree across the async execution boundary. **We add no
hand-rolled spans**; we lean on the framework's tree and add bespoke spans only if a concrete gap
is proven.

## We own the provider; the framework inherits it

The `obs` package builds and globally registers **one** tracer provider per process; the framework
attaches its auto-spans to that global. We **do not** call the framework's own telemetry-setup
helper in any port — registering ours first is the one conflict-free integration that works
uniformly (one port's helper refuses to override an already-registered global; the others read the
global). Single exporter-config owner ⇒ trivial parity.

Registration (`obs.Init` / equivalent) runs **once**, right after config load in the entrypoint,
and:

1. builds the exporter selected by config,
2. constructs an SDK tracer provider with a resource (`service.name`, `service.version`), a batch
   span processor, and the configured sampler,
3. sets it as the OTel **global**, and
4. sets the global **W3C `TraceContext`** propagator.

`Init` returns a `Shutdown` the entrypoint defers. With the default exporter `none`, `Init`
installs nothing and returns a no-op `Shutdown` — the process is unchanged.

## Exporters: `none | console | otlp | gcp` — vendor choice is config, never code

The application speaks exactly four sinks and **never names a vendor**:

| Value | Sink | Notes |
|---|---|---|
| `none` | — | no-op default; merging is a no-op in prod until an environment opts in |
| `console` | spans as text on stdout | local dev; the CLI/playground defaults to this |
| `otlp` | any OTLP-native backend | New Relic, Datadog, Honeycomb, Grafana, Jaeger/Tempo, SigNoz, a Collector — via `OTEL_EXPORTER_OTLP_ENDPOINT` + `OTEL_EXPORTER_OTLP_HEADERS`. **Switching vendors = two env vars, zero code.** |
| `gcp` | Google Cloud Trace | the one convenience exporter: ADC auth, where prod (Vertex/Cloud Run) already lives |

`console`, `gcp`, and `otlp` are three independent viewing surfaces — none privileged. Config
**rejects an unknown value** and **rejects `otlp` with no endpoint** (it would silently export
nowhere).

## The flush constraint (load-bearing, not a tuning knob)

The batch span processor exports **asynchronously** — it buffers a finished span and ships it on a
background timer (~5 s) or when a batch fills. On Cloud Run, CPU is throttled the instant a
response is sent, so the trailing batch is left in the buffer with a starved sender, and the
instance can be reclaimed → **spans silently lost in exactly the prod (scale-to-zero) config.**

This is a **different axis** from what the execution transport fixed: the transport made the
*compute* run in-request (CPU on); it does nothing for *export timing*. The fix leverages that same
in-request window: **force-flush buffered spans before every traced handler returns** — uniformly
in the HTTP middleware (one export per request, **including the fast 202 ingress path**), and again
in `Shutdown`. Export must live inside the request for the same reason the compute does.

(A synchronous/simple span processor needs no flush but blocks on every one of the many spans an
agent run emits — batch processor + one end-of-request flush is the efficient form.)

## Propagation is backend-aware

The trace must cross the enqueue → dispatch hop (a fresh HTTP request via the task queue) so the
workflow trace continues from the ingress span. Propagation mirrors how each transport backend
already moves the envelope:

- **Cloud Tasks backend** → inject the trace context as a **W3C `traceparent` HTTP header** on the
  task (not inside the envelope JSON — the envelope is a versioned cross-port wire contract). The
  server-side HTTP instrumentation on `/internal/dispatch` extracts it, so the dispatch span is a
  child of the ingress span automatically.
- **In-process backend** → there is no HTTP hop, so the worker **inherits the context directly**
  (carrying the span, with cancellation stripped so the dispatch outlives the returned request) —
  no carrier, mirroring how in-process already skips the envelope codec.

The `obs` propagation seam (`Inject` / `Extract`) abstracts both. When tracing is disabled, inject
is a no-op — no `traceparent` leaks onto a task.

## Config

Identical env var names, defaults, and validation across all four ports (parity rule #3). Owned by
each port's `config` layer — the **only** place that reads `OTEL_*`; `obs` takes a typed struct.

| Var | Default | Meaning |
|---|---|---|
| `OTEL_TRACES_EXPORTER` | `none` | `none` · `console` · `otlp` · `gcp` |
| `OTEL_SERVICE_NAME` | `automation-agent` | resource `service.name` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP target; required for `otlp` |
| `OTEL_EXPORTER_OTLP_HEADERS` | — | OTLP auth headers (`k=v,...`); a secret → masked in the config log view |
| `OTEL_TRACES_SAMPLER` | `parentbased_always_on` | standard sampler value |
| `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` | `false` | opt-in capture of prompt/response **bodies** (sensitive); the standard GenAI-semconv var the framework reads natively |

**Message content is off by default.** The flag gates only message **bodies** (prompts/responses =
reviewed source code). The *useful* GenAI attributes — model, prompt/completion token counts, tool
names, finish reason, latency — are captured **free and always on**, so "off" still yields rich,
cost-aware traces. (Body-capture wiring itself is a follow-up; the flag is surfaced now.)

## Log ↔ trace correlation

The existing injected logger is wrapped so records emitted while a span is active also carry
`trace_id` / `span_id`, letting a backend pivot from a log line to its trace (and on the `gcp` path,
the cloud console auto-links them). It reads the active span from the log call's context, so it is
zero-cost when no span is active or tracing is off. Where a port's logger is structured, the ids are
attached as fields; where it is a plain sink, they are appended to the record — same data, whichever
shape the port's logger takes.

## Testing contract (deterministic — no live network, no LLM)

Assert on span **names / attributes / structure**, never on LLM output text. Each port mirrors:

- `none` installs a no-op provider + no-op `Shutdown` (behavior-preserving).
- A recording/in-memory exporter + a fake run emitting one agent-shaped span tree → assert the tree
  shape and GenAI attribute **keys**.
- Propagation round-trip (Cloud Tasks header) **and** passthrough (in-process context) yield the
  same logical trace; the dispatch root continues the ingress trace.
- **Flush on return:** spans are exported **before** the response returns (guards scale-to-zero).
- Config: each exporter value parses/validates; `otlp` without an endpoint is rejected; defaults
  identical across ports.
- Middleware: one server span per request; the health endpoint is excluded.
- Log correlation: an active span → the logger's records carry `trace_id` / `span_id`.
- Arch: `obs` imports no agent package; only `config` reads `OTEL_*`.

## Boundaries

`obs` is deterministic tooling: it imports **no** agent packages (enforced by the arch tests and,
in Go, depguard), and reads **no** environment. Its code and comments name **no** other language
stack (the cross-language-mention rule).
