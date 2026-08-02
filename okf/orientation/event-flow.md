---
type: Architecture
title: Event flow — ingest to workflow to notify
description: How an event travels from ingress (cron, webhooks) through normalization, the execution transport, the root dispatcher, and into a workflow — including the separate resume path for parked fix runs.
tags: [orientation, event-flow, ingress, dispatch]
sensitivity: internal
bundle: automation-agent
status: stable
generated: { by: human:jkjamies, at: 2026-07-04T00:00:00Z }
---

Every piece of work enters through a single front door and is normalized before any agent
sees it. There are two distinct journeys: **kickoff** (an event starts a workflow) and
**resume** (a CI verdict re-enters a parked fix run).

## Kickoff flow

```mermaid
flowchart TD
    CS["Cloud Scheduler (daily)"] --> GW
    GH["GitHub (webhooks, HMAC)"] --> GW
    GW["managed API gateway<br/>(single ingress: authn, rate-limit, route)"] --> Cron["POST /internal/cron/daily"]
    GW --> WLint["POST /webhooks/lint (CI lint report)"]
    GW --> WCov["POST /webhooks/coverage (coverage report)"]
    GW --> WCI["POST /webhooks/github (check_run)"]
    GW --> WReview["POST /webhooks/github (pull_request)"]
    Cron -->|cron.daily| Env["ingest.Envelope{Kind, Source, Payload}"]
    WLint -->|lint| Env
    WCov -->|coverage| Env
    WCI -->|ci| Env
    WReview -->|review| Env
    Env --> TX{"execution transport (TASKS_BACKEND)"}
    TX -->|"inprocess: in-process worker pool (local)"| Root
    TX -->|"cloudtasks: Cloud Tasks → POST /internal/dispatch (in-request)"| Root
    Root["root dispatcher (by Kind)"]
    Root -->|"cron.daily"| Sum["Summary workflow"]
    Root -->|lint| LFK["Lint-fixer: kickoff"]
    Root -->|coverage| CFK["Coverage-fixer: kickoff"]
    Root -->|ci| LFR["Lint/Coverage-fixer: resume (by check name)"]
    Root -->|review| RVK["Reviewer: kickoff (one-shot, in-request)"]

    Sum --> Chat[("Slack / Teams")]
    LFK --> PR[("GitHub PR: automation-agent/* branch + label")]
    CFK --> PR
    PR -->|"agent-*-verify check"| WCI
    RVK --> RV[("GitHub PR: review comments + agent-review check (advisory)")]
```

Key properties:

- **Normalization first.** Every source becomes one `Envelope{Kind, Source, Payload}`
  before routing — see [ingest](/modules/platform/ingest.md). New sources add a `Kind`,
  not a new path.
- **The execution transport** ([tasks](/modules/platform/tasks.md)) decides *where* the
  multi-minute LLM compute runs: an in-process worker pool locally, or Cloud Tasks
  re-entering `POST /internal/dispatch` in production so compute runs in-request on
  allocated CPU (Cloud Run scale-to-zero preserved).
- **One dispatcher.** The [root dispatcher](/modules/agents/root-dispatcher.md) is the
  only place that maps `Kind` → workflow. The webhook layer
  ([webhook](/modules/platform/webhook.md)) verifies signatures and parses; it never
  routes.

## Resume flow (parked fix runs)

A fixer's resume is a separate, later request: the CI check for a parked run reports back
minutes-to-hours after kickoff and re-enters through the same front door as Kind `ci`.
Only the two fixers resume; summary and the reviewer are one-shot.

```mermaid
flowchart TD
    CR["GitHub: check_run completed<br/>(agent-*-verify, on the automation-agent/* branch)"] --> GW["managed API gateway"]
    GW --> WCI["POST /webhooks/github (check_run)"]
    WCI -->|"ci"| TX{"execution transport"}
    TX --> Disp["root dispatcher"] -->|ci| RES["fixer resume(payload)"]

    RES --> NM{"check name == spec check name?"}
    NM -->|no| NOOP["no-op (another engine owns this check)"]
    NM -->|yes| CLAIM["ParkStore resolve-by-PR-key<br/>atomic single-winner claim"]
    CLAIM -->|"late / duplicate / already-claimed / unknown"| NOOP2["no-op (resolved at most once)"]

    CLAIM --> CONC{check_run conclusion}
    CONC -->|success| OK["status-aware summary (success) + clear"]
    CONC -->|"failure & attempts ≥ MaxIter"| REV["status-aware summary (needs review) + clear"]
    CONC -->|"failure & attempts < MaxIter"| RT["resume the parked run → apply again → re-park (attempts+1)"]

    TO["per-run CI_TIMEOUT (soft timer)"] -.->|"CI never reports"| FREE["timeout: claim + summary + clear"]
    SW["/internal/sweep (durable catch-all)"] -.-> FREE

    OK --> Chat[("Slack / Teams")]
    REV --> Chat
    FREE --> Chat
```

Attempts are counted in the park record, **not** from GitHub commits. The atomic claim
(resolve-by-PR-key / sweep) guarantees a late or duplicate webhook racing the timer or
the sweep resolves a run at most once. The full mechanism — including why durability
lives at the session layer — is in
[suspend/resume design](/orientation/suspend-resume-design.md).
