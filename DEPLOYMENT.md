# Deployment & operations — status + checklist

> **Nothing is deployed yet — this repo is code only.** This file is the short status /
> setup checklist. The **canonical deployment + ops reference** (mental model, HTTP hooks,
> auth rationale, full GCP walkthrough, prod-vs-local stack table) is the source of truth
> in [`.agents/standards/deployment.md`](.agents/standards/deployment.md).
>
> Scope: the GCP walkthrough below uses the Go service (`go/`) as the worked example; the
> same durable-sessions design, `SESSION_BACKEND` switch, and env vars apply to every port.
> Per-port parity is tracked per-PR (see [`.agents/standards/language-parity.md`](.agents/standards/language-parity.md)).

## Where to find what

| You want to… | Read |
|---|---|
| Understand the cloud architecture + run the GCP setup | [`.agents/standards/deployment.md`](.agents/standards/deployment.md) |
| Understand the **private-ingress** architecture (gateway → private Cloud Run) | [`.agents/standards/deployment.md` § Private ingress](.agents/standards/deployment.md#private-ingress) |
| Run the agent on your machine (env vars, run modes, container) | [`.agents/standards/local-development.md`](.agents/standards/local-development.md) |
| Run the tests (incl. the Firestore emulator) | [`.agents/standards/testing.md`](.agents/standards/testing.md) |
| Drive the lint/coverage fixers from CI | [`.agents/standards/ci-integration.md`](.agents/standards/ci-integration.md) |

## Setup checklist (to stand one up)

The detailed, copy-paste steps for each item are in
[`.agents/standards/deployment.md`](.agents/standards/deployment.md#gcp-production-setup-step-by-step).

- [ ] Firestore database in **Native mode**.
- [ ] Cloud Run service account: `roles/datastore.user` (+ `roles/aiplatform.user` for
      Gemini-on-Vertex, + `roles/cloudtasks.enqueuer` for the execution transport). ADC is
      automatic on Cloud Run.
- [ ] Secrets in **Secret Manager**: `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET`,
      `INTERNAL_TOKEN`, notifier URL.
- [ ] Deploy `cmd/agent` (`make docker`) to Cloud Run with `SESSION_BACKEND=firestore`,
      `LLM_PROVIDER=gemini`, `TASKS_BACKEND=cloudtasks` (+ the queue vars below), and the
      secrets/`REPOS` as env.
- [ ] **Cloud Tasks queue** (`gcloud tasks queues create <name> --location=<region>`) for the
      in-request execution transport. Set `TASKS_LOCATION`, `TASKS_QUEUE`, and
      `DISPATCH_URL=https://<service>/internal/dispatch` (the queue POSTs here carrying the
      `INTERNAL_TOKEN` Bearer; `TASKS_DISPATCH_DEADLINE` defaults to `30m`). Without this,
      multi-minute compute is throttled after the 202 on scale-to-zero.
- [ ] GitHub **Check runs** webhook → `https://<service>/webhooks/github` (HMAC =
      `GITHUB_WEBHOOK_SECRET`).
- [ ] Two Cloud Scheduler jobs (Bearer `INTERNAL_TOKEN`): `/internal/cron/daily` (the daily
      digest) and `/internal/sweep` (the durable timeout sweep). Cloud Scheduler is the only
      trigger — the service runs no in-process cron.

## TODO (not yet implemented)

- [ ] **Orphan-session GC** for sessions created but never parked.
- [ ] **Terraform/IaC** for Firestore + Cloud Run + Cloud Scheduler + Secret Manager.
- [ ] **CI runs the Firestore emulator** so `*_firestore.go` folds into measured coverage.
- [ ] **Cross-port parity:** keep the ports in lockstep on the durable-session design;
      per-port parity is tracked per-PR (see
      [`.agents/standards/language-parity.md`](.agents/standards/language-parity.md)).
- [ ] **OIDC instead of a shared bearer** for `/internal/*`.
- [ ] **Private ingress** — front the service with a self-hosted API gateway and make Cloud Run
      private so each operator deploys an instance reachable only on their own network. Phased
      rollout below in [Private ingress — what needs to be done](#private-ingress--what-needs-to-be-done).

Full rationale and detail for every item: [`.agents/standards/deployment.md`](.agents/standards/deployment.md).

## Private ingress — what needs to be done

> The target architecture (self-hosted API gateway in front of a **private** Cloud Run, OIDC in
> place of the shared bearer, each operator on their own network) is documented in
> [`.agents/standards/deployment.md` § Private ingress](.agents/standards/deployment.md#private-ingress).
> Nothing here is implemented yet. The fuller design covers the threat model, the per-item
> safety checklist, alternatives considered, and selecting a self-hosted API gateway. Defaults
> (`ingress=all`, `INTERNAL_AUTH=bearer`, daily Cloud Scheduler trigger) reproduce today's public
> behavior, so this is entirely opt-in.

Phased so each step is independently testable:

- [ ] **Phase 0 — private-ingress spike.** Stand up one Cloud Run with
      `ingress=internal-and-cloud-load-balancing` + Internal ALB + serverless NEG; prove a curl
      with an OIDC token works and the public URL is dead. *(No app change.)*
- [ ] **Phase 1 — app auth.** Add `INTERNAL_AUTH` (`bearer`|`oidc`|`both`) + an OIDC verifier
      (signature + `aud` = service URL + allowed SA email) in Go; switch Cloud Scheduler to OIDC;
      mirror to `python/`, `kotlin/`, `javascript/`. Retires `INTERNAL_TOKEN` and folds in the
      existing OIDC TODO above.
- [ ] **Phase 2 — gateway.** Deploy the self-hosted API gateway on the operator network;
      implement the HMAC / GitHub IP-allowlist / replay-window / rate-limit / size-cap policies +
      OIDC mint + mTLS; route `/webhooks/*` (and optionally cron) through it.
- [ ] **Phase 3 — hardening.** VPC Service Controls perimeter around Cloud Run + Firestore +
      Secret Manager; IAM `roles/run.invoker` scoped to **only** the gateway + Scheduler SAs (no
      `allUsers`/`allAuthenticatedUsers`); mTLS in transit; audit-log retention; and (GHE
      Cloud / GitHub public) the hardened public listener + Cloud Armor/WAF.
- [ ] **Phase 4 — docs + IaC.** Fold the adopted topology into `.agents/standards/deployment.md`,
      this file, and `architecture-design.md` §13; add Terraform for the private topology (ties
      into the IaC TODO above).
- [ ] **CodeRabbit review before any commit** (repo hard rule).

**Rollback** is pure config/infra: set Cloud Run `ingress=all`, restore `INTERNAL_AUTH=bearer`,
remove the gateway + Internal ALB, and point GitHub webhooks back at the Cloud Run URL. No data
migration; sessions/park store are unaffected.
