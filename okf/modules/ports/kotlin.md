---
type: Port
title: Kotlin port (frozen)
description: The Kotlin implementation of automation-agent, built on adk-kotlin 0.4.0, forming the feature-frozen pair with the TypeScript port at their current 1:1 behavior.
resource: kotlin/
tags: [kotlin, frozen-pair, adk-kotlin]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Kotlin port (frozen)

The Kotlin implementation under `kotlin/` is built on ADK for Kotlin (`com.google.adk:google-adk-kotlin-core` 0.4.0), the coroutine-based native SDK. Parity with the rest of the system is **functional, not version-matched**. The language-neutral design it implements is [/standards/architecture-design.md](/standards/architecture-design.md).

## Pair membership

Kotlin and the [TypeScript port](/modules/ports/javascript.md) form the **frozen pair**: both are feature-frozen at their current 1:1 behavior with each other, on the ADK 1.x-generation runtime model (long-running suspend/resume). The [Go](/modules/ports/go.md) and [Python](/modules/ports/python.md) ports form the modern pair carrying the design forward on the ADK 2.x line. There is no parity requirement across the two pairs, but external contracts (webhook routes, check names, payloads) match across all four ports. The full parity rules live in [/standards/language-parity.md](/standards/language-parity.md).

## Mental model

Ingest (cron / webhook) → the [root dispatcher](/modules/agents/root-dispatcher.md) → the **summary**, **lintfixer**, or **covfixer** workflow (the two fixers share the `agent.fixflow` engine). The end-to-end system flow and detailed resume flow are diagrammed in the [Go port concept](/modules/ports/go.md) (the reference); this port matches that behavior at its freeze point.

`Main.kt` wires configuration → the model → tooling → the root/summary/fix agents → the webhook server, then blocks until interrupted. `buildTokenProvider` selects the GitHub auth seam (`auth.TokenProvider`): App mode (validated App id, installation id, exactly one private-key source) mints and caches installation tokens; otherwise a static PAT or anonymous. The one provider is adapted to both the REST client's `TokenSource` and the fix engines' git `TokenProvider`, so both share a single cached credential. One `newSessionService` + `newParkStore` pair (selected by `SESSION_BACKEND`: `memory` | `sqlite` | `firestore`) is built at startup and shared by both fix engines, giving them durable suspend/resume; with the memory backend, parked CI-wait runs are abandoned on restart. `POST /internal/sweep` calls every engine's `sweepTimeouts` (collect-and-continue, then rethrow the first failure so Cloud Scheduler retries). The daily digest is driven by Cloud Scheduler calling `POST /internal/cron/daily`; the service runs no internal timer. A `check_run` webhook is handed to every fix engine — each no-ops unless its check name matches.

Webhooks `enqueue` onto the execution transport (`tasks`, selected by `TASKS_BACKEND`): the **in-process** backend (default, local dev) runs each dispatch on a bounded, drained coroutine pool with admission backpressure (a burst blocks at the ingest boundary instead of spawning unbounded coroutines); the **Cloud Tasks** backend (production) hands each envelope to the queue, which POSTs it to `/internal/dispatch` so the workflow runs **in-request** on Cloud Run with durable retry.

## Layout (mirrors the Go reference)

Package root: `com.automation.agent` under `kotlin/src/main/kotlin/...`.

| Kotlin package | Purpose |
|---|---|
| `app` | service entrypoint (`Main.kt`), composition only |
| `playground` | local dev REPL entrypoint (never deployed) |
| `config` | env → typed `Config`; sole reader of the environment (including `OTEL_*`) |
| `ingest` | normalized `Envelope` + `Kind` |
| `notify` | Slack/Teams behind one `Notifier` |
| `githubapi` | GitHub REST tooling |
| `gitrepo` | git working-tree tooling |
| `webhook` | HTTP ingress |
| `tasks` | execution transport (in-process \| Cloud Tasks → `/internal/dispatch`) |
| `obs` | distributed tracing (OpenTelemetry, see [/standards/observability.md](/standards/observability.md)); off by default |
| `auth` | GitHub App / PAT token provider |
| `agent.setup` | LLM builder, Ollama adapter, prompt loader, runner |
| `agent.root` | dispatcher |
| `agent.summary` | commit-digest workflow |
| `agent.lintfixer` | lint-fix workflow |
| `agent.covfixer` | coverage-fix workflow |
| `agent.fixflow` | shared fix engine |
| `agent.reviewer` | PR code-review workflow |

The dedicated `kotlin/konsist/` Gradle module holds the architecture-conformance tests.

## Port-specific quirks

- **Never use `!!`.** The not-null assertion operator is banned (except very exceptional test cases); use `shouldNotBeNull()`, `getValue(...)`, `?:`, `requireNotNull(...)`, or smart-casts instead.
- **Hand-rolled durable session services.** adk-kotlin ships no database session service, so the sqlite and firestore session backends are implemented in this port.
- **Tests are Kotest `BehaviorSpec`** with `Given`/`When`/`Then` blocks (no backtick-named test functions), and test classes/files use the **`Test`** suffix (e.g. `OllamaModelTest`), not `Spec`.
- **Prompts on the classpath:** markdown under `kotlin/src/main/resources/prompts/<agent>/`, loaded from the classpath (the `embed.FS` equivalent).

## Build / run / test

Run from the `kotlin/` directory:

```bash
./gradlew build           # compile + test (service module)
./gradlew test            # unit tests only (Kotest)
./gradlew arch            # architecture conformance (Konsist; :konsist module)
./gradlew koverVerify     # 80% coverage gate
./gradlew run             # run the service
```

## Conventions

Enforced by the `:konsist` module (`./gradlew arch`):

- **Knowledge lives in the bundle** — this port is documented by the concepts in `/modules/` and the standards; the repo-root `AGENTS.md` is the guardrail sheet + pointer.
- **Build-agent pattern:** pure wiring (a `build<Name>Agent` function) is split from testable logic. See [/standards/agent-build-pattern.md](/standards/agent-build-pattern.md).
- **Import boundaries:** tooling (`githubapi`, `gitrepo`, `notify`, `webhook`, `tasks`, `obs`) must not import `agent.*`; provider SDKs (Ollama/Gemini) only in `agent.setup`; nothing outside `app` imports the `app` package; `config` is the only environment reader.
- **Testing:** ≥80% coverage via Kover; never assert on LLM output.
- **Models:** default to local Ollama Gemma; never hardcode a provider in agents.
