# automation-agent (Go / ADK)

This module is an automation service built on the Agent Development Kit (ADK)
(`google.golang.org/adk` v1.4.0). Read [`../.agents/standards/architecture-design.md`](../.agents/standards/architecture-design.md)
first — it is the authoritative design.

## System flow

```mermaid
flowchart TD
    CS["Cloud Scheduler (daily)"] --> GW
    GH["GitHub (webhooks, HMAC)"] --> GW
    GW["managed API gateway<br/>(single ingress: authn, rate-limit, route)"] --> Cron["POST /internal/cron/daily"]
    GW --> WLint["POST /webhooks/lint (CI lint report)"]
    GW --> WCov["POST /webhooks/coverage (coverage report)"]
    GW --> WCI["POST /webhooks/github (check_run)"]
    Cron -->|KindCronDaily| Env["ingest.Envelope{Kind, Source, Payload}"]
    WLint -->|KindLint| Env
    WCov -->|KindCoverage| Env
    WCI -->|KindCI| Env
    Env --> Root["root.Dispatcher.Dispatch (by Kind)"]
    Root -->|"cron.daily"| Sum["Summary workflow"]
    Root -->|lint| LFK["Lint-fixer: Kickoff"]
    Root -->|coverage| CFK["Coverage-fixer: Kickoff"]
    Root -->|ci| LFR["Lint/Coverage-fixer: Resume (by check name)"]

    Sum --> Par["Parallel[fetch_repo x N] -> state commits:<repo>"]
    Par --> Smz["summarize (LLM, OutputKey=digest)"]
    Smz --> Ntf["notify"] --> Chat[("Slack / Teams")]

    LFK -->|"triage -> analyze(parallel/file) -> apply_fix -> await_ci (long-running)"| PR[("GitHub PR: automation-agent/* branch + label")]
    CFK -->|"triage -> explore -> execute -> apply_fix -> await_ci"| PR
    PR -->|"agent-*-verify check"| WCI
    LFR --> Dec{conclusion}
    Dec -->|success| Chat
    Dec -->|"failure & attempts<3"| LFK
    Dec -->|"failure & attempts>=3"| Chat
    TO["per-run CI_TIMEOUT (soft timer + durable /internal/sweep)"] -.->|"CI never reports -> needs review"| Chat

    Models["model.LLM: Ollama/Gemma (local) | Gemini (cloud)"] -.-> Sum
    Models -.-> LFK
```

## Mental model

Ingest (cron / webhook / future hooks) → **root agent** (dispatcher) → one of three
workflow agents: **summary** (commit digests), **lintfixer** (autonomous lint
remediation with a PR + CI loop), or **covfixer** (test-coverage remediation, sharing
the `fixflow` engine). The PR + CI suspend/resume loop runs on ADK long-running tools
plus a `setup.ParkStore` of parked runs. Both the ADK `session.Service` and the
`ParkStore` are selected by `SESSION_BACKEND` (`memory` | `sqlite` | `firestore`,
default `memory`) and built once at startup in `internal/agent/setup`: with a durable
backend (sqlite/firestore) a restart no longer strands in-flight runs; `memory` keeps
the old ephemeral behavior. Deterministic, agent-free tooling lives under `internal/`
and is called by agents but never imports them. For ops, env, and the `/internal/*`
hooks see [`../DEPLOYMENT.md`](../DEPLOYMENT.md).

## Conventions (enforced by `ARCH/` + `make ci`)

- **Every directory has an `AGENTS.md`.** Agent directories use one shared doc
  covering both `agents_setup.go` and `<name>.go`.
- **Build-agent pattern:** `agents_setup.go` is pure wiring (`Build<Name>Agent`);
  `<name>.go` holds testable logic. See `../.agents/standards/agent-build-pattern.md`.
- **Import boundaries:** tooling must not import `internal/agent/...`; provider
  SDKs (Ollama/Gemini) only in `internal/agent/setup`; nothing imports `cmd`.
- **Prompts are markdown** under each agent's `prompts/` dir, loaded via `embed.FS`.
- **Testing:** ≥80% coverage (`make cover`). Never assert on LLM output content.
- **Models:** default to local Ollama Gemma; do not hardcode a provider in agents.

## Working here

- `make help` lists targets. `make ci` is the full local gate (run from this `go/` dir).
- New features/changes get a spec in `../specs/` from a `../.agents/templates` template
  (`make spec name=<slug> kind=<add|remove|change|migrate>`). `specs/` is gitignored.
