# automation_agent/ingest

The normalized `Envelope` that every ingress source is reduced to before reaching
the root agent. `Kind` identifies the trigger (cron.daily, lint, ci);
`Payload` carries the raw source body for the chosen workflow to parse.

## Flow

```mermaid
flowchart TD
    S1["Cloud Scheduler -> POST /internal/cron/daily"] -->|KindCronDaily| N
    W1["webhook:/lint"] -->|KindLint, raw lint JSON| N
    W2[GitHub check_run webhook] -->|KindCI, check_run body| N
    N["new(kind, source, payload, at)"] --> E["Envelope{kind, source, received_at, payload}"]
    E --> V{"k.valid()?"}
    V -->|"cron.daily / lint / ci"| OK[recognized -> route]
    V -->|other| BAD[false -> reject]
    OK --> R[root agent routing]
    R -->|cron.daily| D1[summary digest workflow]
    R -->|lint| D2[lint-fixer workflow]
    R -->|ci| D3[resume lint-fixer]
```

- `envelope.py` — `Envelope`, `Kind`, and `new(...)`.

Adding a new ingress (e.g. Jira) means adding a `Kind` here and a handler that emits
an `Envelope` — the root agent's routing is the only other place that changes.
