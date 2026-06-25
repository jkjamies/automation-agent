# src/webhook

The HTTP ingress. Six routes — a liveness probe, three POST webhooks, and two
Bearer-gated `/internal/*` ingress routes — reduce requests to an `ingest.Envelope` and
hand them to an `IngestFunc` (which should enqueue and return fast):

## Flow

```mermaid
sequenceDiagram
    participant Client
    participant App as "express app (routes)"
    participant Srv as "Server"
    participant Ingest as "IngestFunc"

    Note over App: express route methods<br/>(wrong method -> 404 free)

    rect rgb(235,245,255)
    Client->>App: GET /healthz
    App->>Srv: handle health
    Srv-->>Client: 200 "ok"
    end

    rect rgb(235,255,235)
    Client->>App: POST /webhooks/lint (lint JSON)
    App->>Srv: handle lint(req)
    Srv->>Srv: readBody (5 MiB cap -> 413 over cap)
    alt body over cap
        Srv-->>Client: 413 "request body too large"
    else read error
        Srv-->>Client: 400 "read body"
    else ok
        Srv->>Srv: authenticate (shared HMAC; 401 on mismatch)
        Srv->>Srv: "newEnvelope(Kind.Lint, 'webhook:/lint', body, now())"
        Srv->>Ingest: dispatch -> ingest(env)
        alt rejects
            Ingest-->>Client: 500 "ingest failed"
        else ok
            Ingest-->>Client: 202 Accepted
        end
    end
    end

    rect rgb(245,235,255)
    Client->>App: POST /webhooks/coverage (coverage JSON)
    App->>Srv: handle coverage(req)
    Srv->>Srv: readBody (5 MiB cap -> 413 over cap)
    alt read error
        Srv-->>Client: 400 "read body"
    else ok
        Srv->>Srv: authenticate (shared HMAC; 401 on mismatch)
        Srv->>Srv: "newEnvelope(Kind.Coverage, 'webhook:/coverage', body, now())"
        Srv->>Ingest: dispatch -> ingest(env)
        Ingest-->>Client: 202 Accepted (or 500 on reject)
    end
    end

    rect rgb(255,235,245)
    Client->>App: POST /internal/cron/daily | /internal/sweep (Bearer)
    App->>Srv: internalAuthenticated(req)
    alt no INTERNAL_TOKEN set
        Srv-->>Client: 404 "internal endpoints disabled"
    else missing / wrong Bearer token
        Srv-->>Client: 401 "unauthorized"
    else ok
        alt /internal/cron/daily
            Srv->>Ingest: dispatch Kind.CronDaily -> 202 (or 500)
        else /internal/sweep
            Srv->>Srv: sweep handler (501 if unconfigured)
            Srv-->>Client: 200 (or 500 on sweep error)
        end
    end
    end

    rect rgb(255,245,235)
    Client->>App: POST /webhooks/github (check_run)
    App->>Srv: handle github(req)
    Srv->>Srv: readBody (5 MiB cap -> 413 over cap)
    alt secret set
        Srv->>Srv: verifySignature(secret, X-Hub-Signature-256, body)
        Note right of Srv: HMAC-SHA256, timingSafeEqual
        alt invalid / missing "sha256=" prefix
            Srv-->>Client: 401 "invalid signature"
        end
    end
    Srv->>Srv: "newEnvelope(Kind.CI, 'webhook:/github', body, now())"
    Srv->>Ingest: dispatch -> ingest(env)
    Ingest-->>Client: 202 Accepted (or 500 on reject)
    end
```

- `GET /healthz` — liveness; returns `200 "ok"`.
- `POST /webhooks/lint` — lint-fixer **kickoff** (agnostic lint JSON) -> `Kind.Lint`.
- `POST /webhooks/coverage` — coverage-fixer **kickoff** (coverage JSON) -> `Kind.Coverage`.
- `POST /webhooks/github` — fix-engine **resume** (GitHub `check_run`) -> `Kind.CI`.
- `POST /internal/cron/daily` — Cloud Scheduler trigger for the daily digest -> `Kind.CronDaily`.
- `POST /internal/sweep` — Cloud Scheduler trigger that reconciles every engine's timed-out
  parked runs (the durable timeout backstop).

All three `/webhooks/*` routes share one HMAC over the body (`X-Hub-Signature-256`,
HMAC-SHA256, hex digest), verified in constant time; verification is skipped only when no
`GITHUB_WEBHOOK_SECRET` is set (local dev). Because a kickoff selects the caller-supplied
target repo, lint/coverage are authenticated with the **same** shared secret as the GitHub
resume webhook. The two `/internal/*` routes are instead Bearer-gated by `INTERNAL_TOKEN`:
they return **404** when no token is configured (routes disabled), **401** on a missing or
mismatched Bearer token (compared in constant time), and `/internal/sweep` returns **501**
when no sweep handler is wired.

express returns a 404 for an unmatched method, rejecting the request before it reaches
`ingest`. Each webhook body is read with a 5 MiB cap: oversize bodies are **rejected with
`413`**, not truncated — truncation would break HMAC-SHA256 verification and produce
malformed JSON downstream. Comparisons use `node:crypto`'s `timingSafeEqual`. Deterministic
tooling — no agent imports. Fully tested with `supertest`.
