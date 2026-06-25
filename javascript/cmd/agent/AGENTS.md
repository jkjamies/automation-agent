# cmd/agent

The service entrypoint. Responsibilities:

```mermaid
flowchart TD
    Main["run(): load .env -> config.load()"] --> Cfg
    Cfg -->|err| Fatal["log + exit(1)"]
    Cfg --> LLM["buildLLM(cfg) + buildCodeLLM(cfg)"]
    LLM --> GH["new Client(githubToken)"]
    GH --> Notif["buildNotifier(cfg) -> Notifier | null"]
    Notif --> SumA["buildSummary daily (null if no repos/notifier)"]
    SumA --> Eng["newLintEngine(FixDeps)\nnewCoverageEngine(FixDeps)\nengines = [lint, cov]"]
    Eng --> Disp["buildRootDispatcher(Deps{summaryDaily,\nlintKickoff, coverageKickoff,\nciResume -> every engine})"]
    Disp --> Web["new Server(ingest -> bounded+tracked safeDispatch,\nsecret, internalToken, sweep)"]
    Web --> Listen["app.listen(port) + HTTP timeouts"]
    Listen --> Block["run until SIGINT/SIGTERM"]
    Block --> Shutdown["server.close(); drain in-flight"]
```

1. Load `config`.
2. Build the LLMs (`src/agent/setup`), tooling, and the root + summary agents plus the
   lint-fixer and coverage-fixer `fixflow` engines.
3. Start the webhook HTTP server (Express, with header/request/idle timeouts). Webhook
   dispatches run on a bounded pool (a permit is acquired before the 202) and every
   dispatch is tracked. The daily digest is driven by Cloud Scheduler calling
   `POST /internal/cron/daily`; the service runs no internal timer.
4. Run until interrupted, then close the server and drain in-flight dispatches (bounded by
   a 15s deadline) before exiting.

The fix loop suspends/resumes on ADK long-running tools backed by an injected `ParkStore`
(`SESSION_BACKEND`: memory | sqlite | firestore), with a per-run `CI_TIMEOUT` bounding each
wait. Cloud Scheduler also calls `POST /internal/sweep`, the durable timeout backstop that
reconciles parked runs whose soft timer was lost to a restart.

Keep this module thin — it is composition only. Anything testable belongs in `src/`.
