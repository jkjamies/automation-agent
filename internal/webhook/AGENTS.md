# internal/webhook

The HTTP ingress. Two endpoints reduce requests to an `ingest.Envelope` and hand
them to an `IngestFunc` (which should enqueue and return fast):

- `POST /webhooks/lint` — lint-fixer **kickoff** (agnostic lint JSON) → `KindLint`.
- `POST /webhooks/github` — lint-fixer **resume** (GitHub `check_run`) → `KindCI`,
  HMAC-verified via `X-Hub-Signature-256` when a secret is configured.
- `GET /healthz` — liveness.

Go 1.22 method-pattern routing gives 405s for free. Bodies are size-capped.
Deterministic tooling — no agent imports. Fully tested with `httptest`.
