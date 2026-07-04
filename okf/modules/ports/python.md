---
type: Port
title: Python port
description: The Python implementation of automation-agent, built on google-adk 2.x, forming the modern pair with the Go reference port on the ADK 2.x line.
resource: python/
tags: [python, modern-pair, adk-2x]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Python port

The Python implementation under `python/` is an automation service built on the Agent Development Kit for Python (`google-adk` 2.x, pinned `>=2.0.0` in `python/pyproject.toml`). The language-neutral design it implements is [/standards/architecture-design.md](/standards/architecture-design.md).

## Pair membership

Python and the [Go port](/modules/ports/go.md) form the **modern pair**: both run on the ADK 2.x line and carry the design forward (workflow graphs, request-input pause/resume). The [Kotlin](/modules/ports/kotlin.md) and [TypeScript](/modules/ports/javascript.md) ports form the frozen pair; there is no parity requirement across the two pairs, but external contracts (webhook routes, check names, payloads) match across all four ports. The full parity rules live in [/standards/language-parity.md](/standards/language-parity.md).

## Mental model

Ingest (Cloud Scheduler / webhook) → the [root dispatcher](/modules/agents/root-dispatcher.md) (`root.Dispatcher.dispatch` by `Kind`) → one of the workflow agents: **summary** (commit digests, posted to Slack/Teams), **lintfixer** (autonomous lint remediation with a PR + CI loop), or **covfixer** (coverage remediation; shares the `fixflow` engine with the lint-fixer). The end-to-end system flow and detailed resume flow are diagrammed in the [Go port concept](/modules/ports/go.md) (the reference); this port matches it.

The fix loop suspends across the CI wait (ADK long-running suspend/resume). Both the ADK session and the parked run (`ParkRecord` with `session_id`, `pr_key`, `call_id`, `attempts`, `params`, `parked_at`) are persisted through `SESSION_BACKEND` (`memory` | `sqlite` | `firestore`) via `setup.ParkStore`, so a durable backend resumes in-flight runs after a restart; the default `memory` backend stays ephemeral. A `check_run` webhook is handed to every fix engine — each no-ops unless its check name matches. The claim (`resolve_by_pr_key` / `sweep`) is atomic and single-winner: a late or duplicate webhook racing the per-run `CI_TIMEOUT` timer or the durable `/internal/sweep` catch-all resolves a run at most once. Attempts are counted in the `ParkRecord`, not from GitHub commits.

Webhook-triggered work is handed to the **execution transport** (`python/automation_agent/tasks`, switched by `TASKS_BACKEND`): `inprocess` runs it in an in-process worker pool (local/default), while `cloudtasks` (prod) enqueues to Cloud Tasks → `POST /internal/dispatch` so multi-minute compute runs **in-request** on Cloud Run (CPU stays allocated; scale-to-zero preserved). Deterministic, agent-free tooling lives under `python/automation_agent/` and is called by agents but never imports them.

## Layout

- `python/cmd/agent/` — the service entrypoint (`main.py`). Composition only: loads `.env` (optional, via python-dotenv) + `config.load()`, builds the LLMs (`setup.build_llm` / `build_code_llm`), the `githubapi.Client`, the notifier, the summary agent, and the lint/coverage `fixflow` engines; then the root dispatcher, the execution transport, and the webhook HTTP server (FastAPI + uvicorn). The daily digest is driven by Cloud Scheduler calling `POST /internal/cron/daily`; the service runs no internal timer. On shutdown it drains the transport and closes the park store.
- `python/cmd/playground/` — the local dev web UI entrypoint (never deployed).
- `python/automation_agent/` — all business logic: `config` (sole env reader), `ingest`, `notify`, `githubapi`, `gitrepo`, `webhook`, `tasks`, `obs` (OpenTelemetry, see [/standards/observability.md](/standards/observability.md)), `auth`, and `agent/{setup,root,summary,lintfixer,covfixer,fixflow,reviewer}`.
- `python/arch/` — architecture-conformance tests (pytest + the stdlib `ast` module, no other deps).
- `python/tests/` — the test suite.

## Port-specific quirks

- **`cmd/` is intentionally NOT an importable package.** The service is run with `python cmd/agent/main.py`; a top-level `cmd` package would shadow the stdlib `cmd` module. The `automation_agent` package itself is installed into the virtualenv (`make build`).
- The local model path goes through ADK's `LiteLlm` wrapper (Ollama/Gemma); the cloud path is Gemini. Provider selection lives entirely in `automation_agent/agent/setup`.

## Build / run / test

Run from the `python/` directory; `make help` lists all targets.

- `make build` — install the project + dependencies into the virtualenv; `make run` — run the service; `make playground` — local ADK web UI at :8080.
- `make test` — all tests; `make cover` — coverage gate (≥80% over `automation_agent/`; `cmd` is composition-only); `make cover-firestore` — Firestore-backed tests against a running emulator (needs `FIRESTORE_EMULATOR_HOST`).
- `make lint` (ruff), `make typecheck` (mypy), `make fmt`, `make tidy`, `make ollama-check`, `make docker`.
- `make arch` — architecture conformance (`pytest arch/`).
- `make ci` — the full local gate: `lint typecheck arch test cover`.

## Conventions

Enforced by `python/arch/` + `make ci`:

- **Knowledge lives in the bundle** — this port is documented by the concepts in `/modules/` and the standards; the repo-root `AGENTS.md` is the guardrail sheet + pointer.
- **Build-agent pattern:** `agents_setup.py` is pure wiring (`build_<name>_agent`); the logic files hold the testable behavior. See [/standards/agent-build-pattern.md](/standards/agent-build-pattern.md).
- **Import boundaries** (arch tests): tooling (`automation_agent/{githubapi,gitrepo,webhook,notify}`) must not import `automation_agent.agent...`; provider SDKs (`litellm`/`lite_llm`, `google.adk.models.Gemini`, `google.genai`) only in `automation_agent/agent/setup`; nothing imports `cmd/...`.
- **Prompts are markdown** under each agent's `prompts/` dir, loaded via `importlib.resources`.
- **Testing:** ≥80% coverage; never assert on LLM output content.
- **Models:** default to local Ollama Gemma (`LiteLlm`); never hardcode a provider in agents.
