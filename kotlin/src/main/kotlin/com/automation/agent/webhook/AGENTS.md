# webhook

The HTTP ingress: each request is reduced to a normalized `ingest.Envelope` and handed to an
`IngestFunc` that should enqueue and return quickly. Deterministic tooling — **no agent
imports**.

## Endpoints

- `GET  /healthz` → `ok`
- `POST /webhooks/lint` → `Kind.LINT` kickoff
- `POST /webhooks/coverage` → `Kind.COVERAGE` kickoff
- `POST /webhooks/github` → `Kind.CI` resume
- `POST /internal/cron/daily` → `Kind.CRON_DAILY` (Cloud Scheduler drives the daily digest)
- `POST /internal/sweep` → runs the durable parked-run timeout reconciler (`SweepFunc`)

## Auth model

- **Webhook POSTs** (`/webhooks/{lint,coverage,github}`) all verify the `X-Hub-Signature-256` HMAC
  when a secret is configured (skipped when empty — local dev only). A lint/coverage kickoff selects
  the caller-supplied target repo, so it is authenticated with the same secret as the GitHub webhook,
  not just `/webhooks/github`.
- **Internal POSTs** (`/internal/*`) are Bearer-gated by `INTERNAL_TOKEN`: an unset token disables
  the routes entirely (404); a mismatched `Bearer <token>` is 401 (constant-time compared). When the
  reconciler is wired but `/internal/sweep` has no `SweepFunc`, it returns 501.

## Details

- `Server.kt` — built on **Ktor** (CIO engine). `Application.webhookRoutes(ingest, secret, now,
  internalToken, sweep)` installs the routes; `webhookServer(port, …)` returns an embedded server for
  the entrypoint. `IngestFunc` and `SweepFunc` are `suspend` fun-interfaces; an ingest exception maps
  to 500. Bodies are capped at 5 MiB (413 when exceeded). Ktor returns 405 for a known path with the
  wrong method.
- `Signature.kt` — `verifySignature` uses `javax.crypto.Mac` (HmacSHA256) with a
  constant-time compare (`MessageDigest.isEqual`).

Tested with Ktor's `testApplication` harness (no real socket).
