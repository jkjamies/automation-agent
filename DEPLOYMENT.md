# Deployment & operations — status + checklist

> **Nothing is deployed yet — this repo is code only.** This file is the short status /
> setup checklist. The **canonical deployment + ops reference** (mental model, HTTP hooks,
> auth rationale, full GCP walkthrough, prod-vs-local stack table) is the source of truth
> in [`.agents/standards/deployment.md`](.agents/standards/deployment.md).
>
> Scope: the Go service (`go/`). The Python / TS / Kotlin ports mirror the design but are
> not yet updated for durable cloud sessions (see **TODO: parity**).

## Where to find what

| You want to… | Read |
|---|---|
| Understand the cloud architecture + run the GCP setup | [`.agents/standards/deployment.md`](.agents/standards/deployment.md) |
| Run the agent on your machine (env vars, run modes, container) | [`.agents/standards/local-development.md`](.agents/standards/local-development.md) |
| Run the tests (incl. the Firestore emulator) | [`.agents/standards/testing.md`](.agents/standards/testing.md) |
| Drive the lint/coverage fixers from CI | [`.agents/standards/ci-integration.md`](.agents/standards/ci-integration.md) |

## Setup checklist (to stand one up)

The detailed, copy-paste steps for each item are in
[`.agents/standards/deployment.md`](.agents/standards/deployment.md#gcp-production-setup-step-by-step).

- [ ] Firestore database in **Native mode**.
- [ ] Cloud Run service account: `roles/datastore.user` (+ `roles/aiplatform.user` for
      Gemini-on-Vertex). ADC is automatic on Cloud Run.
- [ ] Secrets in **Secret Manager**: `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET`,
      `INTERNAL_TOKEN`, notifier URL.
- [ ] Deploy `cmd/agent` (`make docker`) to Cloud Run with `SESSION_BACKEND=firestore`,
      `LLM_PROVIDER=gemini`, and the secrets/`REPOS` as env.
- [ ] GitHub **Check runs** webhook → `https://<service>/webhooks/github` (HMAC =
      `GITHUB_WEBHOOK_SECRET`).
- [ ] Three Cloud Scheduler jobs (Bearer `INTERNAL_TOKEN`): `/internal/cron/daily`,
      `/internal/cron/weekly`, `/internal/sweep`. **Disable the in-process cron** to avoid
      double-firing digests (see the caution in the standards doc).

## TODO (not yet implemented)

- [ ] **Orphan-session GC** for sessions created but never parked.
- [ ] **Disable the in-process scheduler** via a flag (e.g. `SCHEDULER=external`).
- [ ] **Terraform/IaC** for Firestore + Cloud Run + Cloud Scheduler + Secret Manager.
- [ ] **CI runs the Firestore emulator** so `*_firestore.go` folds into measured coverage.
- [ ] **Parity:** mirror the durable-session design to Python / TS / Kotlin.
- [ ] **OIDC instead of a shared bearer** for `/internal/*`.

Full rationale and detail for every item: [`.agents/standards/deployment.md`](.agents/standards/deployment.md).
