# cmd/agent

The service entrypoint. Responsibilities (built out across phases):

## Flow

```mermaid
flowchart TD
    Main["main(): slog logger -> run(logger)"] --> Sig["signal.NotifyContext(SIGINT, SIGTERM)"]
    Sig --> Env["godotenv.Load() (.env optional)"]
    Env --> Cfg["config.Load()"]
    Cfg -->|err| Fatal["return err -> os.Exit(1)"]
    Cfg --> LLM["setup.BuildLLM(ctx, cfg)"]
    LLM --> GH["githubapi.New(cfg.GitHubToken)"]
    GH --> Notif["buildNotifier(cfg) -> notify.New (or nil)"]
    Notif --> SumA["buildSummaryAgent (nil if no repos/notifier)"]
    SumA --> Fixer["lintfixer.NewFixer(Deps{LLM, GH, Notify, Label, CheckName, MaxIter})"]
    Fixer --> Disp["root.BuildRootDispatcher(Deps{<br/>SummaryAgent, LintKickoff=Kickoff,<br/>LintResume=Resume})"]
    Disp -->|err| Fatal

    Disp --> Sched["scheduler.New(emit -> dispatcher.Dispatch)"]
    Sched --> AddCron["Add(CronDaily, KindCronDaily)<br/>Add(CronWeekly, KindCronWeekly)"]
    AddCron --> Web["webhook.New(ingest -> go Dispatch,<br/>WithGitHubSecret)"]
    Web --> HTTP["http.Server{Addr ':'+Port, ReadHeaderTimeout 10s}"]
    HTTP --> Rec["reconcile.New(gh, notifier,<br/>resume -> fixer.HandleResume, Config{Repos,Label,CheckName,CITimeout})"]

    Rec --> Start["sched.Start()"]
    Start --> RecLoop["go runReconcileLoop(ctx, reconciler, ReconcileInterval)<br/>(scan on startup + ticker)"]
    RecLoop --> Listen["go httpServer.ListenAndServe()"]
    Listen --> Block["<-sigCtx.Done() (block until signal)"]
    Block --> Shutdown["defer sched.Stop()<br/>httpServer.Shutdown(10s ctx)"]
```

1. Load `config`.
2. Build the LLM (`internal/agent/setup`), tooling, and the root/summary/lintfixer
   agents + runner.
3. Start the scheduler (cron) and the webhook HTTP server.
4. Run the reconcile loop and block until shutdown.

Keep this file thin — it is composition only. Anything testable belongs in
`internal/`. Phase 1 only loads config and logs startup.
