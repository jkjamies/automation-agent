# tasks

The execution transport between webhook ingress and the dispatcher. Webhook ingress reduces a
request to an `ingest.Envelope` and calls `Transport.enqueue`, which returns fast; the envelope's
workflow runs later. Deterministic tooling — **no agent imports** (the dispatcher is injected as a
`DispatchFunc`).

## Why it exists

On Cloud Run with request-based billing, CPU is throttled to near-zero once a response is sent, so
the old post-202 background dispatch starved multi-minute LLM compute. The webhook now **enqueues
and returns fast**; in production the workflow runs **in-request** via `POST /internal/dispatch`,
which Cloud Tasks delivers to (durable retry + queue rate-limiting, CPU stays allocated). See
`specs/20260626-workflow-execution-transport.md`.

## Backends (`TASKS_BACKEND`)

- **`InProcess`** (default, local dev) — runs each dispatch on a bounded coroutine pool. A burst
  applies backpressure (a permit `Semaphore`), and a clean SIGTERM drains in-flight work via
  `close()`. It does **not** survive an instance being reclaimed mid-run — that is exactly why
  production uses Cloud Tasks. The drain is guarded by a **recheck-after-acquire** under a `Mutex`:
  after a parked enqueue acquires a permit it rechecks the closed flag and backs out (release +
  reject) if shutdown began while it was parked, so a late enqueue can never slip past `close()`'s
  drain snapshot. The `EnqueueOptions` hints are ignored here (Cloud Tasks features).
- **`CloudTasks`** (production) — enqueues each envelope as an HTTP-target task pointed at
  `/internal/dispatch`, carrying the wire-encoded envelope as the body and `INTERNAL_TOKEN` as a
  `Bearer` header, with an explicit per-task dispatch deadline and an enqueue-time ~1 MiB size guard.
  The queue gives durable retry with backoff and rate limiting (its `max-concurrent-dispatches`
  replaces the in-process semaphore). The Cloud Tasks client is isolated behind the one-method
  `Submitter` seam so task-building is unit-tested with no live gRPC.

## Files

- `Transport.kt` — `Transport` (`enqueue`/`close`), `DispatchFunc`, `EnqueueOptions` (dedup `name`
  + schedule `delay`).
- `InProcess.kt` — the bounded coroutine-pool backend + `DEFAULT_MAX_CONCURRENT`.
- `CloudTasks.kt` — the Cloud Tasks backend, the `Submitter` seam, `newCloudTasks(...)`, and
  `MAX_TASK_BYTES`.

The wire codec the Cloud Tasks body uses (`ingest.encode`/`ingest.decode`) lives in `ingest`; the
in-process backend passes the `Envelope` object directly and never serializes.
