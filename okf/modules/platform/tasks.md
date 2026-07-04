---
type: Platform-Package
title: Execution Transport
description: The Enqueue transport between webhook ingress and the dispatcher, with in-process and Cloud Tasks backends so long LLM compute runs in-request on Cloud Run.
resource: go/internal/tasks
tags: [transport, cloud-tasks, dispatch]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Execution Transport

The execution transport between [webhook](/modules/platform/webhook.md) ingress and the
dispatcher. Webhook ingress reduces a request to an
[ingest](/modules/platform/ingest.md) `Envelope` and calls `Transport.Enqueue`, which
returns fast; the envelope's workflow runs **later** — and, in production,
**in-request** so Cloud Run keeps CPU allocated for the whole (multi-minute LLM)
compute.

## Why this exists

On Cloud Run with request-based billing, CPU is throttled to near-zero once the response
is sent. The old design ran each dispatch in a background task *after* the 202, so a
long compute was starved and the instance could be reclaimed mid-run. Cloud Tasks is the
primitive that fixes it: **durable retry with backoff**, **rate limiting** (the queue's
`max-concurrent-dispatches`), and an **explicit in-request HTTP target**.

## Backends (config-switched via `TASKS_BACKEND`, like `SESSION_BACKEND`)

```mermaid
flowchart TD
    W[webhook ingress] -->|Enqueue| T{Transport}
    T -->|inprocess default| G["background goroutine pool\n(semaphore + drain on SIGTERM)"]
    T -->|cloudtasks prod| Q["Cloud Tasks queue\nPOST /internal/dispatch\n(Bearer INTERNAL_TOKEN, envelope as body)"]
    Q --> H["/internal/dispatch handler"]
    H -->|in-request, CPU allocated| D[dispatcher.Dispatch]
    G -->|background, throttled after 202| D
```

- **`InProcess`** (default, local dev) — reproduces the pre-transport behavior exactly:
  a bounded background worker pool (Go port: goroutines behind a semaphore), with `Close`
  draining in-flight work. Not durable; a reclaim loses work — which is why prod uses
  Cloud Tasks.
- **`CloudTasks`** (production) — enqueues each envelope as an HTTP-target task pointed at
  `/internal/dispatch`, carrying the JSON-encoded envelope as the body and the static
  `INTERNAL_TOKEN` as a Bearer header (the same auth that endpoint already enforces). The
  real client is isolated behind the one-method `submitter` interface so task-building is
  unit-tested without a live gRPC connection. Each task sets an **explicit dispatch deadline**
  (`TASKS_DISPATCH_DEADLINE`, default/max `30m`) — the HTTP-target default is only 10m, so a
  longer workflow would be cancelled mid-run and retried, duplicating side effects.

## Hints (Options)

`WithName` (Cloud Tasks dedup, ~1h) and `WithDelay` (schedule delay, e.g. a review
debounce) are *optional* and Cloud-Tasks-only. The transport stays deliberately dumb:
coalesce-to-latest / staleness logic lives in the workflow, not here.

## Boundaries

Deterministic tooling — **no agent imports**. The dispatcher is injected as a
`DispatchFunc`, and the envelope codec lives in [ingest](/modules/platform/ingest.md)
(the wire contract). The `/internal/dispatch` worker handler lives in
[webhook](/modules/platform/webhook.md) next to the other `/internal/*` endpoints.
