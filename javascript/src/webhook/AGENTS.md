# src/webhook

The HTTP ingress. Four routes — a liveness probe plus three POST webhooks — reduce
requests to an `ingest.Envelope` and hand them to an `IngestFunc` (which should enqueue
and return fast):

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
    Srv->>Srv: readBody (truncate to 5 MiB cap)
    alt read error
        Srv-->>Client: 400 "read body"
    else ok
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
    Srv->>Srv: readBody (truncate to 5 MiB cap)
    alt read error
        Srv-->>Client: 400 "read body"
    else ok
        Srv->>Srv: "newEnvelope(Kind.Coverage, 'webhook:/coverage', body, now())"
        Srv->>Ingest: dispatch -> ingest(env)
        Ingest-->>Client: 202 Accepted (or 500 on reject)
    end
    end

    rect rgb(255,245,235)
    Client->>App: POST /webhooks/github (check_run)
    App->>Srv: handle github(req)
    Srv->>Srv: readBody (truncate to 5 MiB cap)
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
- `POST /webhooks/github` — fix-engine **resume** (GitHub `check_run`) -> `Kind.CI`,
  HMAC-verified via `X-Hub-Signature-256` when a secret is configured.

express returns a 404 for an unmatched method, rejecting the request before it reaches
`ingest`. Each body is read with a 5 MiB cap: oversize bodies are **truncated** to the
cap and still accepted, not rejected. The HMAC over the body uses
HMAC-SHA256 with a hex digest, compared in constant time via `node:crypto`'s
`timingSafeEqual`. Deterministic tooling — no agent imports. Fully tested with `supertest`.
