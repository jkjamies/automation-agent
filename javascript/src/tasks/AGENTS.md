# src/tasks

The execution transport between webhook ingress and the dispatcher. Webhook ingress reduces
a request to an `Envelope` and calls `Transport.enqueue`, which returns fast; the envelope's
workflow runs **later** — and, in production, **in-request** so Cloud Run keeps CPU allocated
for the whole (multi-minute LLM) compute. See
`specs/20260626-workflow-execution-transport.md`.

## Why this exists

On Cloud Run with request-based billing, CPU is throttled to near-zero once the response is
sent. The old design ran each dispatch in a detached promise *after* the 202, so a long
compute was starved and the instance could be reclaimed mid-run. Cloud Tasks is the primitive
that fixes it: **durable retry with backoff**, **rate limiting** (the queue's
`max-concurrent-dispatches`), and an **explicit in-request HTTP target**.

## Backends (config-switched via `TASKS_BACKEND`, like `SESSION_BACKEND`)

```mermaid
flowchart TD
    W[webhook ingress] -->|enqueue| T{Transport}
    T -->|inprocess default| G["detached promise pool\n(permit semaphore + drain on SIGTERM)"]
    T -->|cloudtasks prod| Q["Cloud Tasks queue\nPOST /internal/dispatch\n(Bearer INTERNAL_TOKEN, envelope as body)"]
    Q --> H["/internal/dispatch handler"]
    H -->|in-request, CPU allocated| D[dispatcher.dispatch]
    G -->|detached, throttled after 202| D
```

- **`InProcess`** (default, local dev) — reproduces the pre-transport behavior exactly: a
  bounded permit pool (backpressure under burst), `close()` drains in-flight work. Single
  event loop, so the drain safety is a **recheck-after-acquire** guard: an enqueue parked on a
  permit when `close()` begins backs out (releases its slot and throws) once it acquires,
  rather than spawn a dispatch the drain has already snapshotted past (the Go reference uses a
  second `select` on its closed channel; JS needs no lock). Not durable; a reclaim loses work —
  which is why prod uses Cloud Tasks.
- **`CloudTasks`** (production) — enqueues each envelope as an HTTP-target task pointed at
  `/internal/dispatch`, carrying the JSON-encoded envelope as the body and the static
  `INTERNAL_TOKEN` as a Bearer header (the same auth that endpoint already enforces). The real
  `@google-cloud/tasks` client is isolated behind the one-method `Submitter` interface so
  task-building is unit-tested without a live gRPC connection. Each task sets an **explicit
  dispatch deadline** (`TASKS_DISPATCH_DEADLINE`, default/max `30m`) — the HTTP-target default
  is only 10m, so a longer workflow would be cancelled mid-run and retried, duplicating side
  effects. An enqueue-time ~1 MiB size guard refuses an oversize envelope up front.

## Hints (`EnqueueOptions`)

`name` (Cloud Tasks dedup, ~1h) and `delayMs` (schedule delay, e.g. a review debounce) are
*optional* and Cloud-Tasks-only. The transport stays deliberately dumb: coalesce-to-latest /
staleness logic lives in the workflow, not here (spec Decision §3).

## Boundaries

Deterministic tooling — **no agent imports**. The dispatcher is injected as a `DispatchFunc`,
and the envelope codec lives in `src/ingest` (the wire contract). The `/internal/dispatch`
worker handler lives in `src/webhook` next to the other `/internal/*` endpoints.

- `transport.ts` — `Transport` / `DispatchFunc` / `EnqueueOptions` / `Logger`.
- `inprocess.ts` — `InProcess` (default/local).
- `cloudtasks.ts` — `CloudTasks`, the `Submitter` seam, and `newCloudTasks(...)` (production).
