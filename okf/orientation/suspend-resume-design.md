---
type: Architecture
title: Suspend/resume design — the durable CI wait
description: Why and how a fix run parks across a 20–40+ minute CI wait on scale-to-zero infrastructure — the workflow-graph pause, the durable session store, the ParkStore claim, and the timers that free abandoned runs.
tags: [orientation, suspend-resume, durability, fixflow]
sensitivity: internal
bundle: automation-agent
status: stable
generated: { by: human:jkjamies, at: 2026-07-04T00:00:00Z }
---

The hardest constraint in the system: a fix cannot be confirmed until CI reports —
20–40 minutes per attempt, often longer, up to ~2 h wall-clock across retries — and the
service runs on Cloud Run with scale-to-zero. No goroutine, coroutine, or event-loop task
can own that wait, because the process owning it may not exist by the time CI answers.

The resolution is a strict split between **framework** and **infrastructure**:

## What the framework owns (in-process)

The fix loop is a deterministic **workflow graph**:

```
Start → apply_fix ─"fix_applied"→ await_ci (request-input pause)
  apply_fix ─default→ conclude    (clean, or the attempt errored)
  await_ci  ─"failure"→ apply_fix (conditional retry cycle)
  await_ci  ─default→ conclude
```

- `apply_fix` performs one attempt (triage → edit → commit → push → ensure PR) and emits
  its outcome as the node's routing event. An attempt error becomes an `"error"` output —
  never a node failure — so the run concludes and a human is notified rather than the
  failure vanishing into a failed run.
- `await_ci` pauses on a **request-input interrupt** with an invocation-derived interrupt
  id. A paused run is *passive data*: the engine writes the paused state into ordinary
  session events and the invocation ends. Nothing runs while parked — which is exactly
  what scale-to-zero requires.
- Resume is a later runner turn carrying a function response targeted at the parked
  interrupt id; the engine rebuilds the paused graph state entirely from the persisted
  session events, re-runs `await_ci` (re-entry mode), and routes on the CI conclusion.
- A replayed (stale or duplicate) interrupt no-ops at the engine level — defense in depth
  behind the ParkStore claim below.

## What infrastructure owns (survives the process)

Two provider-switched stores, both selected by one `SESSION_BACKEND` env
(`memory` | `sqlite` | `firestore`), both confined to the
[setup package](/modules/agents/setup.md). Firestore is the cloud backend because it
matches the service's shape — serverless, per-operation billing, idles at zero alongside
scale-to-zero Cloud Run, and the ADK session model maps onto it directly; a managed
agent runtime backed by a relational store was rejected as an always-provisioned
footprint for a workload that idles most of the day.

- The **ADK session service** — the paused run's event history, from which the engine
  reconstructs graph state on resume. `firestore` is the cloud path (a custom
  implementation; its whole-event JSON encoding round-trips the workflow event fields,
  guarded by a dedicated test). `sqlite` is durable-local; `memory` is the
  zero-dependency default (a restart strands parked runs).
- The **ParkStore** — the park record: `owner/repo#pr → session id`, the parked interrupt
  id, the attempt count, `updated_at`, and the run's serialized params (never
  model-controlled, so nothing in a run's history can redirect which repo or branch is
  edited). Its **atomic single-winner claim** (resolve-by-PR-key / sweep) is the only
  guard against stale or duplicate CI webhooks — a run resolves at most once.

Time while parked is also infrastructure's job. Two layers free a run whose CI never
reports: a soft per-run `CI_TIMEOUT` timer (in-process, lost on restart) and the durable
catch-all sweep (`POST /internal/sweep`, driven by Cloud Scheduler), which claims stale
*parked* records atomically and notifies for human review.

That endpoint runs a second, different pass as well — `SweepOrphans`, which reaps records
nothing can resolve and notifies **no one**. The distinction is the point: a parked run
timing out means a human is waiting on a PR, while an orphan is a run that is already dead.
It is described under [Terminal hygiene](#terminal-hygiene) below.

## Terminal hygiene

Every terminal path — success, retries exhausted, timeout, apply failure, clean/no-work —
sends a status-aware summary to Slack/Teams and then **clears the run**: the ADK session is
deleted and then the park record, so durable backends never accumulate finished runs. That
order is deliberate. The record is the only thing that leads back to the session, so
deleting it first and then failing would strand a session nothing references; keeping the
record when the session delete fails leaves a marker the orphan sweep retries.

Not every run reaches a terminal path, and the ones that don't are invisible to all of the
above: a run whose instance was reclaimed mid-apply never parked, and a run displaced by a
redelivered kickoff had its PR key taken by the newer run. No webhook can resolve either,
and the timeout sweep only looks at *parked* records. So the same `/internal/sweep` also
reaps records that are unparked and older than `ORPHAN_TTL`. It notifies no one — an orphan
is a run that is already dead, not a human waiting on a PR — and it is worth doing because a
park record carries the whole kickoff report, so leaking them is not free.

## Why this line is permanent

The durable wait outlives any process, so it can never be owned by an in-process
framework — any framework. The workflow engine's pause replaced hand-rolled sequencing
*inside* the process; the ParkStore, webhook resume, transport, and sweep are unchanged
by framework choice. Full mechanics, flow diagrams, and store contracts:
[architecture & build plan](/standards/architecture-design.md) and
[fixflow](/modules/agents/fixflow.md).
