# scheduler

Turns cron schedules into `ingest.Envelope`s so the root agent treats time-based triggers
like any other ingress. Deterministic tooling — **no agent imports**.

## Details

- `Scheduler.kt` — `Scheduler(emit, now)` registers schedules via `add(spec, kind)` and runs
  each on its own coroutine.
- Cron parsing uses [cron-utils](https://github.com/jmrozanec/cron-utils) (5-field UNIX
  cron); the `@every <duration>` form (e.g. `@every 1s`) is handled directly. Invalid specs
  throw `IllegalArgumentException`.
- `trigger(kind)` is separated from the loop so it is unit-testable without waiting for a
  real schedule. `start()` is non-blocking; `stop()` cancels the scheduler's coroutine scope.
- `EmitFunc` is a fun-interface receiving the `Envelope` on each fire.
