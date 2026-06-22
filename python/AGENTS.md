# automation-agent (Python / ADK)

This package is an automation service built on the Agent Development Kit (ADK).
Read [`.agents/standards/architecture-design.md`](../.agents/standards/architecture-design.md) first — it is the
authoritative design.

## System flow

```mermaid
flowchart TD
    Cron["scheduler (APScheduler cron 09:00 daily / Mon)"] -->|"KindCronDaily/Weekly"| Env["ingest.Envelope{Kind, Source, Payload}"]
    WLint["POST /webhooks/lint (CI lint report)"] -->|KindLint| Env
    WCov["POST /webhooks/coverage (coverage report)"] -->|KindCoverage| Env
    WCI["POST /webhooks/github (check_run, HMAC)"] -->|KindCI| Env
    Env --> Root["root.Dispatcher.dispatch (by Kind)"]
    Root -->|"cron.*"| Sum["Summary workflow"]
    Root -->|lint| LFK["Lint-fixer: kickoff"]
    Root -->|coverage| CFK["Coverage-fixer: kickoff"]
    Root -->|ci| LFR["Fix engines: resume (each no-ops unless its check matches)"]

    Sum --> Par["Parallel[fetch_repo x N] -> state commits:<repo>"]
    Par --> Smz["summarize (LLM, output_key=digest)"]
    Smz --> Ntf["notify"] --> Chat[("Slack / Teams")]

    LFK -->|"triage -> analyze(parallel/file) -> apply_fix"| PR[("GitHub PR: fix branch + label")]
    CFK -->|"triage -> analyze -> apply_fix"| PR
    PR -->|"check_run completed"| WCI
    LFR --> Dec{conclusion}
    Dec -->|success| Chat
    Dec -->|"failure & attempts<max_iter"| LFK
    Dec -->|"failure & attempts>=max_iter"| Chat
    TO["per-run CI_TIMEOUT timer (in-memory)"] -.->|"CI never reports"| Chat

    Models["BaseLlm: Ollama/Gemma via LiteLlm (local) | Gemini (cloud)"] -.-> Sum
    Models -.-> LFK
    Models -.-> CFK
```

## Mental model

Ingest (cron / webhook / future hooks) -> **root agent** (dispatcher) -> one of three
workflow agents: **summary** (commit digests), **lintfixer** (autonomous lint
remediation with a PR + CI loop), or **covfixer** (coverage remediation; shares the
fixflow engine with the lint-fixer). Deterministic, agent-free tooling lives under
`automation_agent/` and is called by agents but never imports them.

## Conventions (enforced by `arch/` + `make ci`)

- **Every directory has an `AGENTS.md`.** Agent directories use one shared doc
  covering both `agents_setup.py` and the testable logic files.
- **Build-agent pattern:** `agents_setup.py` is pure wiring (`build_<name>_agent`);
  the logic files hold the testable behavior. See `../.agents/standards/agent-build-pattern.md`.
- **Import boundaries:** tooling must not import `automation_agent.agent...`; provider
  SDKs (LiteLlm/Gemini/genai) only in `automation_agent/agent/setup`; nothing imports `cmd`.
- **Prompts are markdown** under each agent's `prompts/` dir, loaded via `importlib.resources`.
- **Testing:** >=80% coverage (`make cover`). Never assert on LLM output content.
- **Models:** default to local Ollama Gemma (`LiteLlm`); do not hardcode a provider in agents.

## Working here

- `make help` lists targets. `make ci` is the full local gate.
- Lint/type-check via `ruff` + `mypy`; coverage measured over `automation_agent/`.
- New features/changes get a spec in `specs/` (gitignored).
