# app

The service entrypoint (`Main.kt`). Composition only — keep it thin; anything testable belongs in
a feature package under `com.automation.agent`.

## Flow

```mermaid
flowchart TD
    Cfg["Config.load()"] --> LLM["buildLLM / buildCodeLLM"]
    Cfg --> AUTH["buildTokenProvider (App vs PAT)"]
    AUTH --> GH["githubapi.Client (+ adapters)"]
    AUTH --> FIX
    Cfg --> GH
    Cfg --> N["newNotifier (optional)"]
    LLM --> SUM["buildSummaryAgent (if repos + notifier)"]
    LLM --> FIX["lintfixer / covfixer engines"]
    GH --> FIX
    SUM --> DISP["buildRootDispatcher(RootDeps)"]
    FIX --> DISP
    DISP --> WH["webhookServer: enqueue -> dispatch"]
    WH --> RUN["server.start(wait = true)"]
```

`main()` wires configuration → the model → tooling → the root/summary/fix agents → the
webhook server, then blocks until interrupted. `buildTokenProvider` selects the GitHub auth seam
(`auth.TokenProvider`): App mode (a validated App id, installation id, and exactly one private-key
source) mints/caches installation tokens, otherwise a static PAT (or anonymous). The one provider is adapted to the REST client's `TokenSource`
and the fix engines' git `TokenProvider`, so both share a single cached credential. One `newSessionService` + `newParkStore` pair
(selected by `SESSION_BACKEND`: memory/sqlite/firestore) is built here and shared by both fix
engines, giving them durable suspend/resume. `POST /internal/sweep` is wired to a `SweepFunc` that
calls every engine's `sweepTimeouts` (collect-and-continue across all engines, then rethrow the first
failure so Cloud Scheduler retries). The daily digest is driven by Cloud Scheduler calling
`POST /internal/cron/daily`; the service runs no internal timer. The summary
workflow is enabled only when repositories and a notifier are configured; the fix engines run
without a notifier (they just won't post results). A check_run webhook is handed to every fix engine
— each no-ops unless its check name matches.

Webhook/cron deliveries are dispatched asynchronously under a 32-permit dispatch semaphore: a permit
is acquired at ingest before the dispatch coroutine launches (admission backpressure — a burst blocks
at the boundary instead of spawning unbounded coroutines) and released when the dispatch finishes. A
shutdown hook drains those in-flight dispatches, then closes the park store's backing connection
(`parkStore.close()` — a no-op for the memory backend). With the memory backend, parked CI-wait runs
are abandoned on restart; the durable backends persist them.

The interactive local REPL lives in the separate [`playground`](../playground/AGENTS.md) entrypoint.
