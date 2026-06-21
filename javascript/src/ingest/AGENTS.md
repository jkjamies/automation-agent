# src/ingest

The normalized `Envelope` that every ingress source is reduced to before reaching
the root agent. `Kind` identifies the trigger (cron.daily, cron.weekly, lint, coverage,
ci); `payload` carries the raw source body for the chosen workflow to parse.

```mermaid
flowchart TD
    S1[scheduler 09:00] -->|CronDaily| N
    S2[scheduler Mon 09:00] -->|CronWeekly| N
    W1["webhook:/lint"] -->|Lint, raw lint JSON| N
    W2["webhook:/coverage"] -->|Coverage, raw coverage JSON| N
    W3[GitHub check_run webhook] -->|CI, check_run body| N
    N["newEnvelope(kind, source, payload, at)"] --> E["Envelope{kind, source, receivedAt, payload}"]
    E --> V{"kindValid(k)?"}
    V -->|"cron.daily / cron.weekly / lint / coverage / ci"| OK[recognized -> route]
    V -->|other| BAD[false -> reject]
```

- `envelope.ts` — `Envelope`, `Kind`, `kindValid()`, and `newEnvelope(...)`.

Adding a new ingress (e.g. Jira) means adding a `Kind` here and a handler that emits
an `Envelope` — the root agent's routing is the only other place that changes.
