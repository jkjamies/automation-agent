---
type: Standard
title: Security model
description: The service's security controls in one view — ingress authentication per route class, credential lifecycle and token-off-disk rules, secret storage, and input hardening.
tags: [security, auth, hmac, secrets, threat-model]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Security model

The service's security controls in one view. Each control is owned and detailed by its
concept; this registry exists so a security review doesn't have to reassemble the model
from package docs. External contracts here bind **all four ports**.

## Ingress authentication

| Surface | Guard | Detail |
|---|---|---|
| `POST /webhooks/*` | **HMAC** `X-Hub-Signature-256` (`GITHUB_WEBHOOK_SECRET`) | Verified on every webhook POST when the secret is set — including the `/webhooks/lint` and `/webhooks/coverage` kickoffs, because a kickoff selects a **caller-supplied target repo**. Bad/missing signature → 401. See [HTTP ingress](/modules/platform/webhook.md). |
| `POST /internal/*` | **Bearer** (`INTERNAL_TOKEN`) | Cloud Scheduler (cron, sweep) and the Cloud Tasks worker (`/internal/dispatch`) present the shared bearer; the endpoints are **disabled (404)** unless the token is set. Bearer-not-OIDC is a recorded trade-off with an upgrade path — see [Deployment](/standards/deployment.md) and [GitHub App auth & native triggers](/decisions/github-app-auth-and-native-triggers.md). |
| `GET /healthz` | none | liveness only. |

A single generic managed **API gateway** fronts Cloud Run (authn, rate-limit, routing);
committed docs never name a specific product. The private-ingress variant (fully
private Cloud Run behind a self-hosted gateway presenting OIDC) is described in
[Deployment](/standards/deployment.md).

## Credentials

- **GitHub**: production authenticates as a **GitHub App** — short-lived (~1h)
  installation tokens, cached and refreshed, pinned to one installation per deployment.
  A PAT exists only as the local-dev fallback. See [auth](/modules/platform/auth.md).
- **Tokens never touch disk**: git operations receive the token as in-memory transport
  auth; it is never embedded in remote URLs and never persisted to `.git/config` (a
  tokened clone URL is reset to the clean URL immediately). Plaintext `http://` remotes
  are refused. See [gitrepo](/modules/platform/gitrepo.md).
- **Secrets live in Secret Manager** in production (`GITHUB_TOKEN` fallback,
  `GITHUB_WEBHOOK_SECRET`, `INTERNAL_TOKEN`, the App private-key PEM, the notifier URL)
  and reach the service as env vars; only the [config](/modules/platform/config.md)
  layer reads the environment.
- The App **private key** is parsed and validated at startup (PKCS#1 and PKCS#8), so a
  bad key fails fast rather than cryptically at the first token exchange.

## Input hardening

- Webhook bodies are **capped at 5 MiB** (reject, not truncate — a truncated body would
  only fail HMAC later); oversized → 413.
- Every inbound event is reduced to the typed [ingest](/modules/platform/ingest.md)
  `Envelope`; unknown kinds and malformed payloads are rejected at the boundary. On the
  Cloud Tasks path, a **poison envelope** (permanently undecodable) is acked and
  dropped, while unexpected errors surface as retried 5xx — a bug never silently
  discards work.
- The Cloud Tasks worker target must be `https://` — the task carries the bearer token,
  which `http://` would leak.

## Boundaries that limit blast radius

- Provider SDKs are confined to the `agent/setup` layer; tooling packages never import
  agents; only config reads the environment — all ARCH-enforced (see
  [Architecture rules](/standards/architecture.md)).
- The reviewer is **advisory and comment-only**; fixers act through PRs that humans
  merge — no agent pushes to a default branch.
