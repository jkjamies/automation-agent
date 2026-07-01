# automation-agent (TypeScript / ADK)

A long-running automation service built on the [Agent Development Kit for
JavaScript](https://github.com/google/adk-js) (`@google/adk`). It ingests events
(cron + webhooks), routes each through a **root dispatcher**, and runs three workflow
agents:

- **Summary** — a daily digest of recent commits across N repos, posted to
  Slack or Teams.
- **Lint-fixer** — consumes an agnostic lint payload, opens a PR with a fix, and loops
  on CI feedback (bounded by `MAX_ITERATIONS`) before posting a result.
- **Coverage-fixer** — consumes an agnostic coverage report and opens a PR adding tests
  for meaningful uncovered logic, with the same CI loop. Shares the `fixflow` engine
  with the lint-fixer.

Local-first on **Ollama + Gemma** via a small `BaseLlm` adapter, with a config switch to
**Gemini/Vertex** for cloud deployment. The PR + CI suspend/resume loop rides on ADK
long-running tools plus an injected `ParkStore` selected by `SESSION_BACKEND`
(`memory` | `sqlite` | `firestore`); with a durable backend parked runs survive a restart,
and the periodic `/internal/sweep` reconciles runs whose timeout timer was lost. Webhook
work runs through an **execution transport** (`TASKS_BACKEND`): an in-process worker pool
locally, or Cloud Tasks → `POST /internal/dispatch` in production so the multi-minute
compute runs in-request on Cloud Run (CPU stays allocated; scale-to-zero preserved).

## Quick start

```bash
cp .env.example .env      # then edit
make help                 # list all targets
make ci                   # lint + typecheck + arch + test + coverage gate
make run                  # run the service
make playground           # local ADK web UI at http://localhost:8080 (dev only)
```

## Layout

| Path | Purpose |
|---|---|
| `cmd/agent` | service entrypoint |
| `cmd/playground` | local ADK web UI (dev only; never deployed) |
| `src/agent` | root / summary / lintfixer / covfixer agents + shared `setup` + `fixflow` |
| `src/{githubapi,gitrepo,webhook,notify,tasks,obs}` | deterministic tooling (`tasks` = execution transport: in-process \| Cloud Tasks; `obs` = distributed tracing, off by default) |
| `src/{config,ingest}` | configuration + normalized event envelope |
| `arch/` | architecture-conformance tests |

Every directory carries an `AGENTS.md`; the arch suite enforces it.

## Toolchain

Node ≥20 (the app runs directly from TypeScript via `tsx`, no build step). `tsc --noEmit`
type-checks, `eslint` lints, `vitest` tests with an 80% coverage floor, and `prettier`
formats. `make ci` is the full local gate.
