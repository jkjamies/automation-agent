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
    GH --> Notif["buildNotifier(cfg) -> notify.New (or nil)"]
    Notif --> SumA["buildSummaryAgent (nil if no repos/notifier)"]
    SumA --> Eng["lintfixer.NewEngine(fixflow.Deps{LLM, CodeLLM,<br/>GH, Notify, Token, MaxIter, CITimeout})<br/>covfixer.NewEngine(same Deps)<br/>engines = [lint, cov]"]
    Eng --> Disp["root.BuildRootDispatcher(Deps{<br/>SummaryAgent,<br/>LintKickoff=lintEngine.Kickoff,<br/>CoverageKickoff=covEngine.Kickoff,<br/>CIResume=resume to every engine})"]
    Disp -->|err| Fatal

    Disp --> Sched["scheduler.New(emit -> dispatcher.Dispatch)"]
    Sched --> AddCron["Add(CronDaily, KindCronDaily)<br/>Add(CronWeekly, KindCronWeekly)"]
    AddCron --> Web["webhook.New(ingest -> go Dispatch,<br/>WithGitHubSecret)"]
    Web --> HTTP["http.Server{Addr ':'+Port, ReadHeaderTimeout 10s}"]

    HTTP --> Start["sched.Start()"]
    Start --> Listen["go httpServer.ListenAndServe()"]
    Listen --> Block["<-sigCtx.Done() (block until signal)"]
    Block --> Shutdown["defer sched.Stop()<br/>httpServer.Shutdown(10s ctx)"]
```

1. Load `config`.
2. Build the LLM + code LLM (`internal/agent/setup`), the `githubapi` client and
   notifier, the summary agent, and the lint-fixer and coverage-fixer `fixflow`
   engines (sharing one `fixflow.Deps`, incl. `CITimeout`).
3. Build the root dispatcher (summary / lint kickoff / coverage kickoff / CI resume),
   then start the scheduler (daily + weekly cron) and the webhook HTTP server.
4. Block until shutdown (SIGINT/SIGTERM), then stop the scheduler and shut the server.

The fix loop is **in-memory and non-durable**: suspend/resume runs on an ADK
long-running `await_ci` tool + `fixflow`'s in-memory parked-run registry, with a per-run
`CI_TIMEOUT` bounding each wait. There is no reconcile loop, so a process restart
strands parked runs.

Keep this file thin — it is composition only. Anything testable belongs in
`internal/`.
