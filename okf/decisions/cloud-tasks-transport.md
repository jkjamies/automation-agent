---
type: Decision
title: Cloud Tasks execution transport
description: Why webhook-triggered LLM compute re-enters the service through a Cloud Tasks queue and runs in-request on Cloud Run, behind a uniform Transport seam.
tags: [decision, transport, cloud-tasks, cloud-run]
sensitivity: internal
bundle: automation-agent
status: accepted
decided: 2026-06-26
timestamp: 2026-07-04T00:00:00Z
---

# Cloud Tasks execution transport

## Context

The service deploys on scale-to-zero Cloud Run with request-based billing: once a
response is sent, CPU is throttled to near-zero and the instance may be reclaimed. The
original ingress design acknowledged a webhook with `202` and ran the workflow in a
background task *after* the response — so a multi-minute LLM workflow was starved of
CPU and could be killed mid-run. This affects **every** webhook-triggered workflow, not
just the long fix loops.

## Decision

Put a uniform **execution transport** seam (`Transport.Enqueue`) between webhook ingress
and the dispatcher, with two config-switched backends (`TASKS_BACKEND`):

- **`inprocess`** (default, local dev) — a bounded background worker pool preserving the
  pre-transport behavior; not durable.
- **`cloudtasks`** (production) — each envelope becomes an HTTP-target task that
  re-enters the service at `POST /internal/dispatch`, so the workflow runs **in-request**
  with CPU allocated, plus durable retry with backoff and queue-level rate limiting.
  Each task sets an explicit dispatch deadline (default/max 30m) because the HTTP-target
  default of 10m would cancel longer workflows mid-run and duplicate side effects.

All workflows go through the transport uniformly — no per-workflow special-casing. The
package concept is [Execution Transport](/modules/platform/tasks.md).

## Consequences

- Long LLM compute is billable in-request but survives on scale-to-zero infrastructure;
  no resident worker and no always-on CPU allocation.
- `/internal/dispatch` joins the bearer-gated internal surface and reuses
  `INTERNAL_TOKEN` (no new auth mechanism).
- The transport stays deliberately dumb: dedup/delay are optional hints; coalescing and
  staleness logic live in the workflows.

## Alternatives considered

- **Always-allocated CPU / min-instances** — rejected: pays for idle capacity around a
  bursty, low-volume workload.
- **Post-202 background execution (status quo)** — rejected: silently broken on
  request-billed Cloud Run; work is throttled and lost on reclaim.
- **A resident worker service** — rejected: reintroduces the always-on infrastructure
  the suspend/resume design exists to avoid.
