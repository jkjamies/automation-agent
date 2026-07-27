---
type: Platform-Package
title: HTTP Ingress
description: The HTTP ingress — HMAC-verified /webhooks/* endpoints, bearer-gated /internal/* Cloud Scheduler triggers, and the Cloud Tasks dispatch worker.
resource: go/internal/webhook
tags: [http, ingress, webhooks]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# HTTP Ingress

The HTTP ingress. The `/webhooks/*` POST endpoints reduce requests to an
[ingest](/modules/platform/ingest.md) `Envelope` and hand them to an `IngestFunc`
(which should enqueue via the [tasks](/modules/platform/tasks.md) transport and return
fast); the `/internal/*` endpoints are the **Cloud Scheduler** ingress (the daily digest +
the durable timeout sweep) plus the **Cloud Tasks** worker (`/internal/dispatch`). Every
`/webhooks/*` POST is HMAC-authenticated with `X-Hub-Signature-256` when a secret is
configured — the `/webhooks/lint` and `/webhooks/coverage` kickoffs as well as
`/webhooks/github`, because a kickoff selects a caller-supplied target repo.

## Flow

```mermaid
sequenceDiagram
    participant Client
    participant Mux as "http.ServeMux (routes())"
    participant Srv as "Server"
    participant Ingest as "IngestFunc"

    Note over Mux: Go 1.22 method-pattern routing<br/>(wrong method -> 405 free)

    rect rgb(235,245,255)
    Client->>Mux: GET /healthz
    Mux->>Srv: handleHealth(w, r)
    Srv-->>Client: 200 "ok"
    end

    rect rgb(235,255,235)
    Client->>Mux: POST /webhooks/lint | /webhooks/coverage (kickoff JSON)
    Mux->>Srv: handleLint / handleCoverage(w, r)
    Srv->>Srv: readBody (MaxBytesReader, ingress cap)
    alt over the cap
        Srv-->>Client: 413 "request body too large"
    else read error
        Srv-->>Client: 400 "read body"
    else secret set & bad/missing signature
        Srv-->>Client: 401 "invalid signature"
    else ok
        Srv->>Srv: "ingest.New(KindLint|KindCoverage, ...)"
        Srv->>Ingest: dispatch -> ingest(ctx, env)
        alt err
            Ingest-->>Client: 500 "ingest failed"
        else ok
            Ingest-->>Client: 202 Accepted
        end
    end
    end

    rect rgb(255,245,235)
    Client->>Mux: POST /webhooks/github (check_run)
    Mux->>Srv: handleGitHub(w, r)
    Srv->>Srv: readBody (MaxBytesReader, ingress cap -> 413 over cap)
    alt secret set
        Srv->>Srv: verifySignature(secret, X-Hub-Signature-256, body)
        Note right of Srv: HMAC-SHA256, hmac.Equal
        alt invalid / missing "sha256=" prefix
            Srv-->>Client: 401 "invalid signature"
        end
    end
    Srv->>Srv: "ingest.New(KindCI, 'webhook:/github', body, now())"
    Srv->>Ingest: dispatch -> ingest(ctx, env)
    Ingest-->>Client: 202 Accepted (or 500 on err)
    end
```

## Endpoints

- `POST /webhooks/lint` — [lint-fixer](/modules/agents/lintfixer.md) **kickoff**
  (agnostic lint JSON) → `KindLint`.
- `POST /webhooks/coverage` — [coverage-fixer](/modules/agents/covfixer.md) **kickoff**
  (agnostic coverage report) → `KindCoverage`.
- `POST /webhooks/github` — lint/coverage-fixer **resume** (GitHub `check_run`) → `KindCI`.
- `GET /healthz` — liveness.
- `POST /internal/cron/daily` — Cloud Scheduler trigger for the daily
  [summary](/modules/agents/summary.md) digest (`KindCronDaily`); lets the schedule live
  GCP-side so Cloud Run scales to zero.
- `POST /internal/sweep` — Cloud Scheduler trigger for the durable timeout sweep
  (`SweepFunc` → `Engine.SweepTimeouts`), the restart-proof catch-all behind the soft timer.
- `POST /internal/dispatch` — the **Cloud Tasks worker** (`DispatchFunc`, wired via
  `WithDispatch`). It decodes the queued `ingest.Envelope` and runs `dispatcher.Dispatch`
  **synchronously, in-request**, so on Cloud Run CPU stays allocated for the whole compute
  (a post-202 background task would be throttled). Because that compute runs for minutes —
  far longer than the server `WriteTimeout` sized for the fast webhook handlers — the handler
  **clears this connection's write deadline** so a slow-but-successful dispatch still delivers
  its 2xx (a lost response would make Cloud Tasks retry completed work). Retry classification
  follows Cloud Tasks'
  retry-on-non-2xx contract: a transient dispatch error → `500` (the queue retries with
  backoff); a poison body (undecodable / unknown `Kind`) → `200` + log (acked so the queue
  drops it instead of looping). Returns `501` when no dispatcher is wired. See
  [tasks](/modules/platform/tasks.md).

## Authentication and limits

The `/webhooks/*` POSTs are HMAC-verified via `X-Hub-Signature-256` when a secret is
configured (skipped only when unset, for local dev) — the kickoffs included, since they
pick the target repo. The `/internal/*` endpoints use a **Bearer token** (`INTERNAL_TOKEN`)
and are **disabled (404)** unless that token is set (`internalAuthenticated`); the Cloud
Tasks transport attaches that same token, so `/internal/dispatch` reuses the check verbatim.
The bearer-vs-OIDC rationale is covered in the
[deployment standard](/standards/deployment.md). Go 1.22
method-pattern routing gives 405s for free.

Bodies are size-capped per route class, and the two caps differ on purpose. An ingress route
(`/webhooks/*`, `/internal/cron/*`) reads at most `ingest.MaxPayloadBytes` — the largest raw
body that still fits in a task once the envelope base64s it — so anything accepted there can
actually be enqueued. `/internal/dispatch` receives that already-encoded envelope, which is
larger, so it reads up to `ingest.MaxEncodedBytes`. Over-cap is `413`, never truncation: a
truncated body would fail HMAC verification and feed malformed JSON downstream, and `413` is
the honest status because the caller must send less — a retry of the same body can never
succeed, whereas a `500` would invite the source to retry it forever.

Deterministic tooling — no agent imports. Tested against a local HTTP harness rather than a
live GitHub, so the routing, auth, and cap behavior are exercised without network.
