# ingest

The normalized `Envelope` that every ingress source is reduced to before reaching the root
agent. `Kind` identifies the trigger (cron.daily, lint, coverage, ci);
`payload` carries the raw source body for the chosen workflow to parse.

## Details

- `Envelope.kt` holds `Kind` (enum; `value` is the wire string, `Kind.from`/`Kind.valid`
  validate it) and `Envelope` (data class with `Envelope.new(...)`).
- `Envelope` overrides `equals`/`hashCode` to give `ByteArray payload` structural value
  semantics.

Adding a new ingress means adding a `Kind` here plus a handler that emits an `Envelope` —
the root agent's routing is the only other place that changes.
