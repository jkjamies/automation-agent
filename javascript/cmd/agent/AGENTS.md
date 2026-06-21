# cmd/agent

The service entrypoint. Responsibilities:

```mermaid
flowchart TD
    Main["run(): load .env -> config.load()"] --> Cfg
    Cfg -->|err| Fatal["log + exit(1)"]
    Cfg --> LLM["buildLLM(cfg) + buildCodeLLM(cfg)"]
    LLM --> GH["new Client(githubToken)"]
    GH --> Notif["buildNotifier(cfg) -> Notifier | null"]
    Notif --> SumA["buildSummary (null if no repos/notifier)"]
    SumA --> Eng["newLintEngine(FixDeps)\nnewCoverageEngine(FixDeps)\nengines = [lint, cov]"]
    Eng --> Disp["buildRootDispatcher(Deps{summaryAgent,\nlintKickoff, coverageKickoff,\nciResume -> every engine})"]
    Disp --> Sched["new Scheduler(emit -> safeDispatch)\nadd(cronDaily/Weekly)"]
    Sched --> Web["new Server(ingest -> safeDispatch, secret)"]
    Web --> Listen["sched.start(); app.listen(port)"]
    Listen --> Block["run until SIGINT/SIGTERM"]
    Block --> Shutdown["sched.stop(); server.close()"]
```

1. Load `config`.
2. Build the LLMs (`src/agent/setup`), tooling, and the root + summary agents plus the
   lint-fixer and coverage-fixer `fixflow` engines.
3. Start the scheduler (croner) and the webhook HTTP server (Express).
4. Run until interrupted, then stop the scheduler and close the server.

The fix loop is non-durable and in-memory (ADK long-running suspend/resume + `fixflow`'s
in-memory parked-run registry, with a per-run `CI_TIMEOUT` bounding each wait); there is
no reconcile loop, so a restart strands parked runs.

Keep this module thin — it is composition only. Anything testable belongs in `src/`.
