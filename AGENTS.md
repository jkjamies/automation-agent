# automation-agent

This repository is an automation service built on the Agent Development Kit (ADK).
The **Go** implementation in [`go/`](go/) is the canonical reference; sibling ports
live in their own top-level folders. Read [`.agents/standards/architecture-design.md`](.agents/standards/architecture-design.md)
first — it is the authoritative, language-neutral design.

## Language parity — two pairs (Go · Python — modern | Kotlin · TypeScript — frozen)

This service is maintained as parallel ports organized as **two parity pairs**:

| Pair | Language | Location | ADK | Status |
|---|---|---|---|---|
| Modern | Go | [`go/`](go/) (`cmd/`, `internal/`) | `google.golang.org/adk/v2` v2.0.0 | reference |
| Modern | Python | [`python/`](python/) | `google-adk` (PyPI, 2.x line) | functional 1:1 with Go — `make ci` green |
| Frozen | Kotlin | [`kotlin/`](kotlin/) | `com.google.adk:google-adk-kotlin-core` 0.4.0 ([adk-kotlin](https://github.com/google/adk-kotlin)) | frozen — 1:1 with TypeScript |
| Frozen | TypeScript | [`javascript/`](javascript/) | `@google/adk` ([adk-js](https://github.com/google/adk-js)) | frozen — 1:1 with Kotlin |

Each language uses its own native ADK; parity is **functional, not version-matched**.
The **modern pair** carries the design forward on the ADK 2.x line (graph workflows,
request-input pause/resume). The **frozen pair** is feature-frozen at its current 1:1
behavior (their ADKs have no 2.x line); a critical fix that must touch one frozen port
lands in both. There is **no parity requirement across the pairs** — but external
contracts (webhook routes, check names, notify payloads) match across all four.

**The parity contract** (full rules: [`.agents/standards/language-parity.md`](.agents/standards/language-parity.md)):

- Go is the source of truth. A behavior change lands in Go first, then is mirrored
  into Python **in the same logical change** — the modern pair never silently drifts.
- Parity is about *observable behavior and structure*, not syntax: same packages/dirs,
  same public surface, same config keys, env vars, defaults, routes, and payloads.
- Each port keeps the same conventions (per-directory `AGENTS.md`, build-agent pattern,
  prompts-as-markdown, ≥80% coverage, no asserting on LLM output).
- When you touch a modern port, check its pair and update it, or record the gap in the
  PR description — parity is tracked per-PR (see
  [`.agents/standards/language-parity.md`](.agents/standards/language-parity.md)).

## System flow

```mermaid
flowchart TD
    CS["Cloud Scheduler (daily)"] --> GW
    GH["GitHub (webhooks, HMAC)"] --> GW
    GW["managed API gateway<br/>(single ingress: authn, rate-limit, route)"] --> Cron["POST /internal/cron/daily"]
    GW --> WLint["POST /webhooks/lint (CI lint report)"]
    GW --> WCov["POST /webhooks/coverage (coverage report)"]
    GW --> WCI["POST /webhooks/github (check_run)"]
    GW --> WReview["POST /webhooks/github (pull_request)"]
    Cron -->|KindCronDaily| Env["ingest.Envelope{Kind, Source, Payload}"]
    WLint -->|KindLint| Env
    WCov -->|KindCoverage| Env
    WCI -->|KindCI| Env
    WReview -->|KindReview| Env
    Env --> TX{"execution transport (TASKS_BACKEND)"}
    TX -->|"inprocess: in-process worker pool (local)"| Root
    TX -->|"cloudtasks: Cloud Tasks → POST /internal/dispatch (in-request)"| Root
    Root["root.Dispatcher.Dispatch (by Kind)"]
    Root -->|"cron.daily"| Sum["Summary workflow"]
    Root -->|lint| LFK["Lint-fixer: Kickoff"]
    Root -->|coverage| CFK["Coverage-fixer: Kickoff"]
    Root -->|ci| LFR["Lint/Coverage-fixer: Resume (by check name)"]
    Root -->|review| RVK["Reviewer: Kickoff (one-shot, in-request)"]

    Sum --> Par["Parallel[fetch_repo x N] -> state commits:<repo>"]
    Par --> Smz["summarize (LLM, OutputKey=digest)"]
    Smz --> Ntf["notify"] --> Chat[("Slack / Teams")]

    LFK -->|"triage -> analyze(parallel/file) -> apply_fix -> await_ci (durable pause)"| PR[("GitHub PR: automation-agent/* branch + label")]
    CFK -->|"triage -> explore -> execute -> apply_fix -> await_ci"| PR
    PR -->|"agent-*-verify check"| WCI
    LFR --> Dec{conclusion}
    Dec -->|success| Chat
    Dec -->|"failure & attempts<3"| LFK
    Dec -->|"failure & attempts>=3"| Chat
    TO["per-run CI_TIMEOUT (soft timer + durable /internal/sweep)"] -.->|"CI never reports -> needs review"| Chat

    RVK -->|"filter -> size-gate -> Parallel[lenses] -> glue -> scorecard -> reconcile/publish"| RV[("GitHub PR: review comments + agent-review check (advisory)")]

    Models["model.LLM: Ollama/Gemma (local) | Gemini (cloud)"] -.-> Sum
    Models -.-> LFK
    Models -.-> RVK
```

## Resume flow (detailed)

The system flow above is kickoff-centric: an event arrives and a workflow *starts*. A
fixer's **resume** is a separate, later request — the CI check for a parked run reports
back minutes-to-hours after kickoff and re-enters through the same front door. Only the
two fixers (lint, coverage) resume; summary and the reviewer are one-shot. This is the
detail behind the system flow's `PR → check_run → Resume` edge, including the durable
`ParkStore` lookup that reconnects the event to its suspended run.

```mermaid
flowchart TD
    CR["GitHub: check_run completed<br/>(agent-*-verify, on the automation-agent/* branch)"] --> GW["managed API gateway (single ingress)"]
    GW --> WCI["POST /webhooks/github (check_run)"]
    WCI -->|"KindCI, check_run body"| Env["ingest.Envelope{Kind, Source, Payload}"]
    Env --> TX{"execution transport (TASKS_BACKEND)"}
    TX -->|"inprocess: in-process worker pool (local)"| Disp
    TX -->|"cloudtasks: Cloud Tasks → POST /internal/dispatch (in-request)"| Disp
    Disp["root.Dispatcher.Dispatch (by Kind)"] -->|ci| RES["fixer.Resume(payload)"]

    RES --> NM{"check name == spec.CheckName?"}
    NM -->|no| NOOP["no-op (another engine owns this check)"]
    NM -->|yes| CLAIM["ParkStore.ResolveByPRKey(owner/repo#pr)<br/>atomic single-winner claim"]
    CLAIM -->|"late / duplicate / already-claimed / unknown"| NOOP2["no-op (resolved at most once)"]

    subgraph Store["Durable park/session store — SESSION_BACKEND: memory | sqlite | firestore"]
        PRK["PRKey index: owner/repo#pr → session id"] --> REC["ParkRecord{SessionID, PRKey, CallID, Attempts, Params, ParkedAt}"]
        REC --> SESS["ADK session (suspended run history)"]
    end
    CLAIM -->|"PRKey → session id"| PRK

    CLAIM --> CONC{check_run conclusion}
    CONC -->|success| OK["status-aware summary (success) + clear:<br/>delete ParkRecord + ADK session"]
    CONC -->|"failure & attempts ≥ MaxIter"| REV["status-aware summary (needs review) + clear"]
    CONC -->|"failure & attempts < MaxIter"| RT["resume ADK session → apply_fix again → re-park (attempts+1)"]
    RT --> SUS(["pause awaiting CI (durable — survives restart)"])
    SUS -.->|"next check_run for this PR"| CR

    TO["per-run CI_TIMEOUT (soft in-process timer, lost on restart)"] -.->|"CI never reports"| FREE["onTimeout: claim + summary + clear"]
    SW["/internal/sweep → SweepTimeouts (durable catch-all)"] -.-> FREE

    OK --> Chat[("Slack / Teams")]
    REV --> Chat
    FREE --> Chat
```

Attempts are counted in the `ParkRecord`, **not** from GitHub commits. Because the claim
(`ResolveByPRKey` / `Sweep`) is atomic and single-winner, a late or duplicate webhook
racing the timeout timer or the sweep resolves the run at most once.

## Mental model

Ingest (cron / webhook / future hooks) → **root agent** (dispatcher) → one of four
workflow agents: **summary** (commit digests), **lintfixer** (autonomous lint
remediation with a PR + CI loop), **covfixer** (test-coverage remediation, sharing
the `fixflow` engine), or **reviewer** (PR code review on the native `pull_request`
event — one-shot/in-request, no suspend/resume; fans out category lenses → glue →
count-based scorecard → an advisory, comment-only review). The PR + CI suspend/resume loop
(fixers only) runs on a durable ADK pause awaiting the CI result
plus a `setup.ParkStore` of parked runs, both backed by `SESSION_BACKEND`
(`memory` | `sqlite` | `firestore`, default `memory`): with a durable backend a restart
no longer strands in-flight runs; `memory` keeps the old ephemeral behavior.
Webhook-triggered work is handed to the **execution transport** (`TASKS_BACKEND`):
`inprocess` runs it in an in-process worker pool (local/default), while `cloudtasks`
(prod) enqueues to Cloud Tasks → `POST /internal/dispatch` so the multi-minute compute
runs **in-request** on Cloud Run (CPU stays allocated; scale-to-zero preserved).
Deterministic, agent-free tooling lives under `internal/` and is called by
agents but never imports them. Env vars + local run modes:
[`.agents/standards/local-development.md`](.agents/standards/local-development.md); ops,
the `/internal/*` hooks, and GCP setup:
[`.agents/standards/deployment.md`](.agents/standards/deployment.md).

## Conventions (enforced by `ARCH/` + `make ci`)

- **Every directory has an `AGENTS.md`.** Agent directories use one shared doc
  covering both `agents_setup.go` and `<name>.go`.
- **Build-agent pattern:** `root` and `summary` split into `agents_setup.go` (pure
  wiring — `BuildRootDispatcher` / `BuildSummaryAgent`) and `<name>.go` (testable logic).
  The `lintfixer`/`covfixer` fixers instead build a `fixflow.Spec` and expose
  `NewEngine(fixflow.Deps)`, sharing the generic `fixflow` engine. See
  `.agents/standards/agent-build-pattern.md`.
- **Import boundaries:** tooling must not import `internal/agent/...`; provider
  SDKs (Ollama/Gemini) only in `internal/agent/setup`; nothing imports `cmd`.
- **Prompts are markdown** under each agent's `prompts/` dir, loaded via `embed.FS`.
- **Testing:** ≥80% coverage (`make cover`). Never assert on LLM output content.
- **Models:** default to local Ollama Gemma; do not hardcode a provider in agents.

## Working here

- `make help` lists targets. `make ci` is the full local gate.
- New features/changes get a spec in `specs/` from a `.agents/templates` template
  (`make spec name=<slug> kind=<add|remove|change|migrate>`). `specs/` is gitignored.
