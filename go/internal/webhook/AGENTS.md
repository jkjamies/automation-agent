# internal/webhook

The HTTP ingress. Three POST endpoints reduce requests to an `ingest.Envelope` and hand
them to an `IngestFunc` (which should enqueue and return fast). Every POST endpoint is
HMAC-authenticated with `X-Hub-Signature-256` when a secret is configured — the
`/webhooks/lint` and `/webhooks/coverage` kickoffs as well as `/webhooks/github`,
because a kickoff selects a caller-supplied target repo.

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
    Srv->>Srv: readBody (MaxBytesReader 5 MiB)
    alt over 5 MiB
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
    Srv->>Srv: readBody (MaxBytesReader 5 MiB -> 413 over cap)
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

- `POST /webhooks/lint` — lint-fixer **kickoff** (agnostic lint JSON) → `KindLint`.
- `POST /webhooks/coverage` — coverage-fixer **kickoff** (agnostic coverage report) → `KindCoverage`.
- `POST /webhooks/github` — lint/coverage-fixer **resume** (GitHub `check_run`) → `KindCI`.
- `GET /healthz` — liveness.

All three POST endpoints are HMAC-verified via `X-Hub-Signature-256` when a secret is
configured (skipped only when unset, for local dev) — the kickoffs included, since they
pick the target repo. Go 1.22 method-pattern routing gives 405s for free. Bodies are
size-capped at 5 MiB (over-cap → `413`, not truncated). Deterministic tooling — no agent
imports. Fully tested with `httptest`.
