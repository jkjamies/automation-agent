---
type: Port
title: TypeScript port (frozen)
description: The TypeScript implementation of automation-agent, built on @google/adk 1.x, forming the feature-frozen pair with the Kotlin port at their current 1:1 behavior.
resource: javascript/
tags: [typescript, frozen-pair, adk-js]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# TypeScript port (frozen)

The TypeScript implementation under `javascript/` is an automation service built on the Agent Development Kit for JavaScript (`@google/adk` 1.x). The app runs directly from TypeScript via `tsx` — **there is no build step**. The language-neutral design it implements is [/standards/architecture-design.md](/standards/architecture-design.md).

## Pair membership

TypeScript and the [Kotlin port](/modules/ports/kotlin.md) form the **frozen pair**: both are feature-frozen at their current 1:1 behavior with each other, on the ADK 1.x-generation runtime model (long-running suspend/resume). The [Go](/modules/ports/go.md) and [Python](/modules/ports/python.md) ports form the modern pair carrying the design forward on the ADK 2.x line. There is no parity requirement across the two pairs, but external contracts (webhook routes, check names, payloads) match across all four ports. The full parity rules live in [/standards/language-parity.md](/standards/language-parity.md).

## Mental model

Ingest (cron / webhook) → the [root dispatcher](/modules/agents/root-dispatcher.md) → one of the workflow agents: **summary** (commit digests posted to Slack/Teams), **lintfixer** (autonomous lint remediation with a PR + CI loop), or **covfixer** (test-coverage remediation, sharing the `fixflow` engine). The end-to-end system flow and detailed resume flow are diagrammed in the [Go port concept](/modules/ports/go.md) (the reference); this port matches that behavior at its freeze point.

The PR + CI suspend/resume loop runs on ADK **long-running tools** plus an injected `ParkStore` selected by `SESSION_BACKEND` (`memory` | `sqlite` | `firestore`); a durable backend lets parked runs survive a restart, and `POST /internal/sweep` (driven by Cloud Scheduler) reconciles runs whose per-run `CI_TIMEOUT` timer was lost to a restart. Webhook-triggered work is handed to the **execution transport** (`javascript/src/tasks`, switched by `TASKS_BACKEND`): `inprocess` runs it on a bounded, drained in-process worker pool (local/default), while `cloudtasks` (prod) enqueues to Cloud Tasks → `POST /internal/dispatch` so multi-minute LLM compute runs **in-request** on Cloud Run (on request-based billing, CPU is throttled after a 202, so the work must run inside a request). Deterministic, agent-free tooling lives under `javascript/src/` and is called by agents but never imports them.

## Layout

- `javascript/cmd/agent/` — the service entrypoint, run as `tsx cmd/agent/main.ts`. Composition only: loads `.env` + `config.load()`, builds the LLMs (`buildLLM` / `buildCodeLLM`), the GitHub token provider (App installation token | PAT/anonymous) and `Client`, the notifier, the summary agent, and the lint/coverage `fixflow` engines; then the root dispatcher, the execution transport, and the webhook HTTP server (Express, with header/request/idle timeouts). The daily digest is driven by Cloud Scheduler calling `POST /internal/cron/daily`; the service runs no internal timer. On SIGINT/SIGTERM it closes the server, drains the transport, and closes the park store.
- `javascript/cmd/playground/` — a local dev REPL over the configured model (never deployed).
- `javascript/src/` — all business logic: `config` (sole env reader), `ingest`, `notify`, `githubapi`, `gitrepo`, `webhook`, `tasks`, `obs` (OpenTelemetry, see [/standards/observability.md](/standards/observability.md)), `auth`, `testutil`, and `agent/{setup,root,summary,lintfixer,covfixer,fixflow,reviewer}`.
- `javascript/arch/` — architecture-conformance tests (Vitest; import scanning, no type-checker needed).

## Port-specific quirks

- **Hand-rolled Ollama adapter:** adk-js ships no Ollama model, so `src/agent/setup/ollama.ts` provides `OllamaLlm extends BaseLlm`, forwarding tool declarations + generation config to a local `/api/chat` endpoint over `fetch`. Agents receive a `BaseLlm` from `setup.buildLLM`; provider selection lives entirely in `src/agent/setup`.
- **No build step:** the app runs from TypeScript via `tsx`; `tsc --noEmit` is the type gate.
- Long-running suspend/resume plumbing (`LongRunDriver`, the deterministic `Sequencer`) lives in `src/agent/setup/longrun.ts` so callers such as `fixflow` stay genai-free.

## Build / run / test

Run from the `javascript/` directory; `make help` lists all targets.

- `make build` — install the project + dependencies; `make run` — run the service; `make playground` — local ADK web UI at :8080.
- `make test` — all tests; `make cover` — coverage gate (≥80% over `src/`; `cmd` is composition-only); `make cover-firestore` — the emulator-gated Firestore tests (needs a running Firestore emulator).
- `make lint` (eslint), `make typecheck` (`tsc --noEmit`), `make fmt`, `make tidy`, `make ollama-check`, `make docker`.
- `make arch` — architecture conformance (Vitest over `arch/`).
- `make ci` — the full local gate: `lint typecheck arch test cover`.

## Conventions

Enforced by `javascript/arch/` + `make ci`:

- **Knowledge lives in the bundle** — this port is documented by the concepts in `/modules/` and the standards; the repo-root `AGENTS.md` is the guardrail sheet + pointer.
- **Build-agent pattern:** `agentsSetup.ts` is pure wiring (`build<Name>Agent`); the logic files hold the testable behavior. See [/standards/agent-build-pattern.md](/standards/agent-build-pattern.md).
- **Import boundaries** (arch tests): tooling must not import `src/agent/...`; provider SDKs (the Ollama adapter / Gemini / `@google/genai`) only in `src/agent/setup`; nothing outside `cmd/` imports `cmd/...`.
- **Prompts are markdown** under each agent's `prompts/` dir, read from disk relative to `import.meta.url`.
- **Testing:** ≥80% coverage; never assert on LLM output content.
- **Models:** default to local Ollama Gemma; never hardcode a provider in agents.
