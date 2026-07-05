---
type: Decision
title: GitHub App auth & native triggers
description: Why production authenticates as a pinned-installation GitHub App with a PAT-only local fallback, CI hooks in via native check_run events, and a generic managed gateway fronts the service.
tags: [decision, github-app, auth, triggers, gateway]
sensitivity: internal
bundle: automation-agent
status: accepted
decided: 2026-06-24
timestamp: 2026-07-04T00:00:00Z
---

# GitHub App auth & native triggers

## Context

Early versions ran on a long-lived personal access token, polled with in-process cron,
and asked target repos to add YAML/curl steps to report CI back. All three aged badly:
a PAT is a broad, non-expiring credential tied to a person; in-process cron dies with
scale-to-zero; and repo-side wiring puts an integration burden on every target repo.

## Decision

- **GitHub App installation tokens in production.** One App, **one pinned installation
  per deployment** (single org): no per-owner cache, no dynamic repo→installation
  resolution; the `TokenProvider.Token(ctx, repo)` seam is the cross-port contract and
  a PAT remains only as the local-dev fallback. Tokens are short-lived (~1h),
  cached/refreshed, supplied to git as in-memory transport auth, and never written to
  `.git/config` or embedded in URLs. See [auth](/modules/platform/auth.md).
- **Native CI hook.** The fix loop resumes on the GitHub `check_run` webhook delivered
  to `POST /webhooks/github` — no YAML or curl step is added to target repos; the
  label-triggered verify workflow reports through GitHub itself. See
  [CI integration](/standards/ci-integration.md).
- **Scheduled work is infrastructure-owned**: Cloud Scheduler hits bearer-gated
  `/internal/*` endpoints (daily digest, timeout sweep); there is no in-process timer.
- **A generic, un-named managed API gateway** is the single ingress in front of Cloud
  Run — committed docs never name a specific gateway product. `/internal/*` uses a
  shared bearer (`INTERNAL_TOKEN`) rather than OIDC for now; the trade-off and upgrade
  path are recorded in [Deployment](/standards/deployment.md).

## Consequences

- The credential is org-scoped, short-lived, and rotates itself; revoking the App
  revokes everything.
- Target repos need zero integration work beyond installing the App and having the
  verify workflow available.
- Single-org-per-deployment is a deliberate constraint; multi-org means multiple
  deployments, not a token cache.

## Alternatives considered

- **PAT in production** — rejected: non-expiring, person-bound, over-scoped.
- **Repo-side CI reporting (YAML/curl in each target repo)** — rejected: per-repo
  integration burden and drift; the native `check_run` event already carries the verdict.
- **Full IAM/OIDC on `/internal/*` now** — deferred, not rejected: GitHub webhooks
  cannot authenticate to Google IAM, so a fully private service conflicts with the
  public webhook surface; the app-validated-OIDC upgrade path is documented in
  [Deployment](/standards/deployment.md).
