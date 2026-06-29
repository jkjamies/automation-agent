# cmd/agent

The service entrypoint. Responsibilities:

```mermaid
flowchart TD
    Main["run(): load .env -> config.load()"] --> Cfg
    Cfg -->|err| Fatal["log + exit(1)"]
    Cfg --> LLM["buildLLM(cfg) + buildCodeLLM(cfg)"]
    LLM --> Prov["buildTokenProvider(cfg): App installation token | PAT/anonymous"]
    Prov --> GH["new Client(provider)"]
    GH --> Notif["buildNotifier(cfg) -> Notifier | null"]
    Notif --> SumA["buildSummary daily (null if no repos/notifier)"]
    SumA --> Eng["newLintEngine(FixDeps)\nnewCoverageEngine(FixDeps)\nengines = [lint, cov]"]
    Eng --> Disp["buildRootDispatcher(Deps{summaryDaily,\nlintKickoff, coverageKickoff,\nciResume -> every engine})"]
    Disp --> Tx["buildTransport(cfg, dispatcher.dispatch):\nInProcess (default) | CloudTasks"]
    Tx --> Web["new Server(ingest -> transport.enqueue,\nsecret, internalToken, sweep,\ndispatch -> /internal/dispatch, log)"]
    Web --> Listen["app.listen(port) + HTTP timeouts"]
    Listen --> Block["run until SIGINT/SIGTERM"]
    Block --> Shutdown["server.close(); transport.close() (drain); parkStore.close()"]
```

1. Load `config`.
2. Build the LLMs (`src/agent/setup`), tooling, and the root + summary agents plus the
   lint-fixer and coverage-fixer `fixflow` engines.
3. Start the webhook HTTP server (Express, with header/request/idle timeouts). Webhooks
   `enqueue` onto the execution transport (`buildTransport`): the **in-process** backend (the
   default, local dev) runs each dispatch on a bounded, drained pool; the **Cloud Tasks**
   backend (production, `TASKS_BACKEND=cloudtasks`) hands each envelope to the queue, which
   POSTs it to `/internal/dispatch` so the workflow runs **in-request** with durable retry —
   on Cloud Run's request-based billing, CPU is throttled after the 202, so long LLM compute
   must run inside a request. The same `dispatcher.dispatch` backs that worker endpoint. The
   daily digest is driven by Cloud Scheduler calling `POST /internal/cron/daily`; the service
   runs no internal timer.
4. Run until interrupted, then close the server, `transport.close()` to drain in-flight
   dispatches (the in-process backend; Cloud Tasks closes its client), and close the park
   store before exiting.

The fix loop suspends/resumes on ADK long-running tools backed by an injected `ParkStore`
(`SESSION_BACKEND`: memory | sqlite | firestore), with a per-run `CI_TIMEOUT` bounding each
wait. Cloud Scheduler also calls `POST /internal/sweep`, the durable timeout backstop that
reconciles parked runs whose soft timer was lost to a restart.

Keep this module thin — it is composition only. Anything testable belongs in `src/`.
