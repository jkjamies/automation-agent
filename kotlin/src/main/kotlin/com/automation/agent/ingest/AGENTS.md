# ingest

The normalized `Envelope` that every ingress source is reduced to before reaching the root
agent. `Kind` identifies the trigger (cron.daily, lint, coverage, ci);
`payload` carries the raw source body for the chosen workflow to parse.

## Details

- `Envelope.kt` holds `Kind` (enum; `value` is the wire string, `Kind.from`/`Kind.valid`
  validate it), `Envelope` (data class with `Envelope.new(...)`), and the wire codec
  (`encode` / `decode`).
- `Envelope` overrides `equals`/`hashCode` to give `ByteArray payload` structural value
  semantics.

Adding a new ingress means adding a `Kind` here plus a handler that emits an `Envelope` —
the root agent's routing is the only other place that changes.

## Wire codec (`encode` / `decode`)

The Cloud Tasks transport (`tasks`) crosses a process boundary: it serializes an envelope to JSON,
hands it to a task body, and `POST /internal/dispatch` deserializes it. `encode`/`decode` are that
codec, and the JSON form is a **cross-port external contract** that must stay **byte-identical**
across all four language ports (Go/Python/JS/Kotlin), so the in-process backend (which passes the
object directly) never calls them.

```json
{"kind":"lint","source":"webhook:/lint","received_at":"1970-01-01T00:00:00Z","payload":"aGk="}
```

- Compact separators (no spaces), field order `kind, source, received_at, payload`.
- `received_at` is RFC 3339 with a trailing `Z` and Go-style trimmed fractional seconds
  (`.000` → none, `.500` → `.5`) — `java.time`'s own ISO formatter groups fractional digits in
  threes and would diverge, so the spelling is built by hand.
- `payload` is **standard base64** of the raw bytes (`""` when empty) — never a raw byte array — so
  an empty payload is the empty string in every port.
- `decode` is strict: an unknown kind, non-canonical/junk base64 (Java's decoder is
  re-encode-checked), a non-string `payload`/`source`, or a present-but-unparseable `received_at`
  is a **permanent (poison) error** (`IllegalArgumentException`). Absent or JSON-`null`
  `source`/`received_at` default to the zero value. Because `Envelope.kind` is a `Kind` enum, an
  unknown kind is unrepresentable on `encode` — the type system enforces what the string-typed ports
  guard at runtime.
