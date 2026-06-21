# webhook

The HTTP ingress: each request is reduced to a normalized `ingest.Envelope` and handed to an
`IngestFunc` that should enqueue and return quickly. Deterministic tooling — **no agent
imports**.

## Endpoints

- `GET  /healthz` → `ok`
- `POST /webhooks/lint` → `Kind.LINT` kickoff
- `POST /webhooks/coverage` → `Kind.COVERAGE` kickoff
- `POST /webhooks/github` → `Kind.CI` resume; verifies `X-Hub-Signature-256` HMAC when a
  secret is configured (skipped when empty — local dev only).

## Details

- `Server.kt` — built on **Ktor** (CIO engine). `Application.webhookRoutes(ingest, secret,
  now)` installs the routes; `webhookServer(port, …)` returns an embedded server for the
  entrypoint. `IngestFunc` is a `suspend` fun-interface; a thrown exception maps to 500.
  Ktor returns 405 for a known path with the wrong method.
- `Signature.kt` — `verifySignature` uses `javax.crypto.Mac` (HmacSHA256) with a
  constant-time compare (`MessageDigest.isEqual`).

Tested with Ktor's `testApplication` harness (no real socket).
