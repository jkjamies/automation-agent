# internal/ingest

The normalized `Envelope` that every ingress source is reduced to before reaching
the root agent. `Kind` identifies the trigger (cron.daily, lint, coverage, ci);
`Payload` carries the raw source body for the chosen workflow to parse.

## Flow

```mermaid
flowchart TD
    S1["Cloud Scheduler -> POST /internal/cron/daily"] -->|KindCronDaily| N
    W1["webhook:/lint"] -->|KindLint, raw lint JSON| N
    W2["webhook:/coverage"] -->|KindCoverage, raw coverage JSON| N
    W3[GitHub check_run webhook] -->|KindCI, check_run body| N
    N["New(kind, source, payload, at)"] --> E["Envelope{Kind, Source, ReceivedAt, Payload}"]
    E --> V{"k.Valid()?"}
    V -->|"cron.daily / lint / coverage / ci"| OK[recognized -> route]
    V -->|other| BAD[false -> reject]
    OK --> R[root agent routing]
    R -->|cron.daily| D1[summary digest workflow]
    R -->|lint| D2[lint-fixer workflow]
    R -->|coverage| D3[coverage-fixer workflow]
    R -->|ci| D4[resume lint/coverage fixer]
```

Adding a new ingress (e.g. Jira) means adding a `Kind` here and a handler that emits
an `Envelope` — the root agent's routing is the only other place that changes.

## Wire codec

`Encode`/`Decode` are the envelope's JSON wire form, used when it crosses the Cloud Tasks
boundary (`internal/tasks` → `POST /internal/dispatch`). The form — `kind`/`source`
strings, `received_at` RFC 3339, `payload` standard base64 — is an external contract and
must stay byte-identical across all four language ports. `Decode` rejects an unknown
`Kind` as a permanent (poison) error so the worker acks rather than retries it. The
in-process transport passes the struct directly and never touches the codec.
