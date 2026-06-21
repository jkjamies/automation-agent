# automation_agent/scheduler

Wraps `APScheduler` (`CronTrigger`) to emit `ingest.Envelope`s on a schedule, so time
triggers flow through the same root-agent path as any other ingress.

## Flow

```mermaid
flowchart TD
    Boot["cmd/agent: new(emit_func)"] --> S["Scheduler{scheduler, emit, now}"]
    S --> AddD["add(CRON_DAILY '0 9 * * *', KindCronDaily)"]
    S --> AddW["add(CRON_WEEKLY '0 9 * * 1', KindCronWeekly)"]
    AddD -->|"add_job(CronTrigger.from_crontab(spec), closure)"| Reg{"valid spec?"}
    AddW -->|"add_job(CronTrigger.from_crontab(spec), closure)"| Reg
    Reg -->|no| Err["raise -> run() aborts"]
    Reg -->|yes| Loop["start(): scheduler.start() (non-blocking)"]
    Loop -->|"each job fire"| Closure["closure -> s.trigger(kind)"]
    Closure --> Tr["trigger(kind)"]
    Tr -->|"ingest.new(kind, 'scheduler', None, now())"| Env["ingest.Envelope{kind, source:'scheduler', payload:None, received_at}"]
    Env --> Emit["emit(env) = EmitFunc"]
    Emit --> Disp["root dispatcher.dispatch(env)"]
    Disp --> C{"kind?"}
    C -->|"cron.daily"| Sum["summary digest"]
    C -->|"cron.weekly"| Sum2["summary digest (Monday)"]
    Stop["stop(): scheduler.shutdown()"] -.->|"jobs cancelled"| Loop
    Entries["entries(): len(scheduler.get_jobs())"] -.->|test assertion| Loop
```

- `add(spec, kind)` registers a 5-field cron spec (e.g. `0 9 * * *` daily,
  `0 9 * * 1` Mondays) via `CronTrigger.from_crontab`.
- `trigger` is factored out of the cron closure so the emit path is unit-testable
  without waiting on real time; `now` is injectable.

Note: the Monday lint trigger is expected to come from an external CI job posting to
`/webhooks/lint`, not from a cron here — see `docs/architecture.md` §8.
