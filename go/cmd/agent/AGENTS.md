# cmd/agent

The service entrypoint. Responsibilities (built out across phases):

## Flow

```mermaid
flowchart TD
    Main["main(): slog logger -> run(logger)"] --> Sig["signal.NotifyContext(SIGINT, SIGTERM)"]
    Sig --> Env["godotenv.Load() (.env optional)"]
    Env --> Cfg["config.Load()"]
    Cfg -->|err| Fatal["return err -> os.Exit(1)"]
    Cfg --> LLM["setup.BuildLLM(ctx, cfg)<br/>setup.BuildCodeLLM(ctx, cfg)"]
    LLM --> GH["githubapi.New(cfg.GitHubToken)"]
    GH --> Sess["setup.NewSessionService(ctx, cfg)<br/>setup.NewParkStore(ctx, cfg)<br/>(SESSION_BACKEND: memory|sqlite|firestore)"]
    Sess --> Notif["buildNotifier(cfg) -> notify.New (or nil)"]
    Notif --> SumA["buildSummaryAgent (nil if no repos/notifier)"]
    SumA --> Eng["lintfixer.NewEngine(fixflow.Deps{LLM, CodeLLM,<br/>GH, Notify, Token, MaxIter, CITimeout,<br/>SessionService, ParkStore})<br/>covfixer.NewEngine(same Deps)<br/>engines = [lint, cov]"]
    Eng --> Disp["root.BuildRootDispatcher(Deps{<br/>SummaryDaily,<br/>LintKickoff=lintEngine.Kickoff,<br/>CoverageKickoff=covEngine.Kickoff,<br/>CIResume=resume to every engine})"]
    Disp -->|err| Fatal

    Disp --> Trans["buildTransport(cfg) -> tasks.Transport<br/>(TASKS_BACKEND: inprocess | cloudtasks)"]
    Trans --> Web["webhook.New(ingest -> transport.Enqueue,<br/>WithGitHubSecret, WithInternalToken,<br/>WithDispatch -> dispatcher.Dispatch,<br/>WithSweep -> engines.SweepTimeouts)"]
    Web --> HTTP["http.Server{Addr ':'+Port, ReadHeaderTimeout 10s}"]

    HTTP --> Listen["go httpServer.ListenAndServe()"]
    Listen --> Block["<-sigCtx.Done() (block until signal)"]
    Block --> Shutdown["httpServer.Shutdown(10s ctx)<br/>transport.Close() (drain / close client)"]
```

1. Load `config`.
2. Build the LLM + code LLM (`internal/agent/setup`), the `githubapi` client, the
   `SESSION_BACKEND`-selected `session.Service` + `setup.ParkStore` (both built once
   here via `setup.NewSessionService`/`setup.NewParkStore`), the notifier, the summary
   agent, and the lint-fixer and coverage-fixer `fixflow` engines (sharing one
   `fixflow.Deps`, incl. `CITimeout`, `SessionService`, `ParkStore`).
3. Build the root dispatcher (summary / lint kickoff / coverage kickoff / CI resume) and
   the execution transport (`buildTransport` → `tasks.Transport`, selected by
   `TASKS_BACKEND`), then start the webhook HTTP server. The webhook `IngestFunc`
   **enqueues** on the transport and returns fast; `WithDispatch` wires
   `dispatcher.Dispatch` to `/internal/dispatch` (the Cloud Tasks worker that runs the
   workflow in-request); `WithInternalToken` + `WithSweep` enable the `/internal/*`
   daily-cron + sweep hooks. The daily digest is driven by Cloud Scheduler calling
   `POST /internal/cron/daily`; the service runs no internal timer.
4. Block until shutdown (SIGINT/SIGTERM), then shut the server and `transport.Close()`
   (the in-process backend drains in-flight dispatches; the Cloud Tasks backend closes its
   client) before the deferred session/park-store closers.

The fix loop's durability follows `SESSION_BACKEND`: suspend/resume runs on an ADK
long-running `await_ci` tool + the injected `setup.ParkStore`, with a per-run
`CI_TIMEOUT` bounding each wait. With a durable backend (`sqlite`/`firestore`) parked
runs survive a restart and the durable `/internal/sweep` is the timeout catch-all; the
default `memory` backend is ephemeral (a restart strands parked runs). See
`../../../DEPLOYMENT.md` for the backends, the `/internal/*` hooks, and ops.

Keep this file thin — it is composition only. Anything testable belongs in
`internal/`.
