# Deployment

The canonical deployment + operations reference. How the service is structured for the
cloud, the HTTP surface, and the step-by-step GCP setup. This is the **source of truth**;
the repo-root [`DEPLOYMENT.md`](../../DEPLOYMENT.md) is a thin status/checklist pointer
back here (no environment is stood up yet — the repo is code only).

> **Scope:** the GCP walkthrough below uses the **Go** reference (`go/`) as the worked
> example; the same design, HTTP surface, and `SESSION_BACKEND` switch apply to every port
> (see [Other ports](#other-ports) for the per-port backend stacks).
>
> Related: [`local-development.md`](local-development.md) (run it on your machine) ·
> [`testing.md`](testing.md) (Firestore emulator) · [`ci-integration.md`](ci-integration.md)
> (driving the fixers from CI) · `.agents/standards/architecture-design.md` §8, §13 (design).

---

## Go (`go/`) — reference

### Mental model

```
 GitHub repo ──webhook(HMAC)──► POST /webhooks/{lint,coverage,github}
 Cloud Scheduler ─bearer─►       POST /internal/cron/daily            (daily digest)
 Cloud Scheduler ─bearer─►       POST /internal/sweep                 (timeout catch-all)
                                         │
                              managed API gateway   (single ingress: authn, rate-limit, routing)
                                         │
                                    Cloud Run service (this app)
                                         │
                       ┌─────────────────┴─────────────────┐
                  session.Service                       ParkStore
                  (suspend/resume history)         (prKey→session, attempts, params)
                  memory | sqlite | firestore     memory | sqlite | firestore
```

The fix loop opens a PR, **suspends** waiting for CI, and **resumes** when GitHub posts
the `check_run` result. With a durable backend (`sqlite` locally, **`firestore`** in prod)
a restart no longer strands in-flight runs — which is what lets Cloud Run scale toward
zero. Eager terminal cleanup deletes both the park record and the session on completion,
so a durable backend doesn't leak. GitHub holds the durable PR artifacts; the agent
doesn't scan them for recovery.

### HTTP hooks (endpoints)

| Method + path | Auth | Purpose |
|---|---|---|
| `GET /healthz` | none | liveness |
| `POST /webhooks/lint` | HMAC (`GITHUB_WEBHOOK_SECRET`) | kick off a lint fix |
| `POST /webhooks/coverage` | HMAC | kick off a coverage fix |
| `POST /webhooks/github` | HMAC | `check_run` event → resume the parked fix |
| `POST /internal/cron/daily` | Bearer (`INTERNAL_TOKEN`) | fire the daily commit digest |
| `POST /internal/sweep` | Bearer | resolve runs whose CI never reported within `CI_TIMEOUT` |

`/internal/*` are **disabled (404)** unless `INTERNAL_TOKEN` is set. HMAC verification is
skipped only when `GITHUB_WEBHOOK_SECRET` is empty (local dev).

#### Why a shared bearer for `/internal/*` (and not OIDC yet)

Cloud Run serves a **public** URL, and the service *must* be public because **GitHub
webhooks can't authenticate to Google IAM** (they only sign with HMAC). So `/webhooks/*`
forces the whole service public, which leaves `/internal/*` reachable by anyone who knows
the URL. Full IAM-OIDC wants a *private* service, which conflicts with the public webhook
surface. So we use a **shared bearer token (`INTERNAL_TOKEN`)** as the pragmatic guard.
Blast radius is modest: `/internal/sweep` only resolves runs *already past `CI_TIMEOUT`*;
the cron endpoints only trigger digests. **Decision: bearer now, OIDC later** — the
cleanest upgrade is *app-validated OIDC* (verify the Google ID token in the handler,
audience-checked), keeping a single service and dropping the shared secret. Tracked under
[Planned hardening](#planned-hardening).

> For a deployment that must stay off the public internet, the [Private
> ingress](#private-ingress) architecture fronts a **private** Cloud Run with a **self-hosted
> API gateway** on the operator's own network: the gateway is the IAM-authenticated caller and
> presents a Google OIDC token to `/internal/*`, so a private service receives webhook-originated
> traffic without a shared bearer.

### Configuration

The full env-var reference lives in
[`local-development.md`](local-development.md#environment-variables-full-reference). The
vars that matter specifically for a **cloud** deploy:

| Var | Prod value |
|---|---|
| `SESSION_BACKEND` | `firestore` (durable, scale-to-zero) |
| `LLM_PROVIDER` | `gemini` (+ `GOOGLE_GENAI_USE_VERTEXAI=TRUE`, `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`) unless a GPU VM runs Ollama |
| `GITHUB_WEBHOOK_SECRET` | **set** — from Secret Manager |
| `INTERNAL_TOKEN` | **set** — from Secret Manager (else cron/sweep are 404) |
| `CI_TIMEOUT` | `90m` (default) — per-run CI wait before the timer/sweep frees a parked run |
| `GITHUB_TOKEN`, notifier URLs | **set** — from Secret Manager |
| `REPOS` | the kickoff allowlist |
| `FIRESTORE_PROJECT` / `FIRESTORE_COLLECTION` | blank = detect from ADC / default `automation_agent` |

### GCP production setup (step by step)

1. **Firestore** — create a database in **Native mode** in your project. No schema/indexes
   to pre-create (single-field queries auto-index).
2. **Auth (ADC)** — give the Cloud Run service account `roles/datastore.user` (Firestore)
   and, for Gemini-on-Vertex, `roles/aiplatform.user`. No keys needed; ADC is automatic on
   Cloud Run.
3. **Build + deploy to Cloud Run.** `make docker` (or `docker build -t automation-agent
   go/`) builds `cmd/agent` only. Deploy with env: `SESSION_BACKEND=firestore`,
   `GOOGLE_CLOUD_PROJECT`, `LLM_PROVIDER=gemini` (+ `GOOGLE_GENAI_USE_VERTEXAI=TRUE`),
   `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET`, `INTERNAL_TOKEN`, `NOTIFY_PROVIDER` + webhook
   URL, `REPOS`. Store secrets in **Secret Manager** and mount them as env — not `.env`.
4. **GitHub webhook** — in each target repo's settings add a webhook →
   `https://<service>/webhooks/github`, content-type `application/json`, secret =
   `GITHUB_WEBHOOK_SECRET`, events: **Check runs**. The lint/coverage kickoffs
   (`/webhooks/{lint,coverage}`) are POSTed by your CI with the same secret in
   `X-Hub-Signature-256` (see [`ci-integration.md`](ci-integration.md)).
5. **Cloud Scheduler** — two jobs, each an HTTP POST with header
   `Authorization: Bearer <INTERNAL_TOKEN>` (or an OIDC token + a tightened handler):

   | Job | Target | Schedule (cron) |
   |---|---|---|
   | daily digest | `POST /internal/cron/daily` | `0 9 * * *` |
   | timeout sweep | `POST /internal/sweep` | e.g. `*/15 * * * *` |

Cloud Scheduler is the only trigger — the service runs no in-process cron, so there is no
double-fire to guard against and `min-instances=0` (scale-to-zero) is safe.

### Prod vs local stack

| Concern | Local | Prod (Cloud Run) |
|---|---|---|
| Compute | `make run`, single process | Cloud Run + `SESSION_BACKEND=firestore` (scale-to-zero); or `min-instances=1` / a GCE VM for the in-memory mode |
| LLM | Ollama (default) | `LLM_PROVIDER=gemini` (Vertex) unless a GPU VM runs Ollama |
| Session + park store | `memory` / `sqlite` | `firestore` (durable; a restart resumes in-flight runs) |
| Secrets | `.env` | Secret Manager mounted as env |
| Scheduler | Cloud Scheduler → `/internal/cron/daily` + `/internal/sweep` (Bearer) | same — Cloud Scheduler is the only trigger; no in-process cron |
| Timeout safety | in-process per-run timer | the timer **and** the durable `/internal/sweep` catch-all |
| HA / scale-out | n/a | `firestore` is a shared store with atomic single-winner claims, so replicas can in principle share it; not exercised yet |

### CI/CD

GitHub Actions builds/pushes the image and deploys to Cloud Run. (IaC is
[planned hardening](#planned-hardening); the setup steps above are manual today.)

### Planned hardening

These pieces are not yet built; they harden a deployment but are not required to stand one up:

- **Orphan-session GC.** The `/internal/sweep` business timeout only sees runs in
  `parked_runs`. A session created but **never parked** (a crash between session-create
  and park) has no park record and leaks (firestore especially). A cleanup hook would
  delete sessions whose `updated_at` is older than a stale threshold
  (≈ `CI_TIMEOUT × MAX_ITERATIONS` + margin, ~6–24h), working for firestore + sqlite, riding
  `/internal/sweep` or a Firestore native TTL policy on `_sessions`.
- **Terraform/IaC** for Firestore + Cloud Run + Cloud Scheduler + Secret Manager.
- **CI running the Firestore emulator** so `*_firestore.go` folds back into measured
  coverage (see [`testing.md`](testing.md)).
- **Cross-port parity** on the durable-session design — kept in lockstep per the parity
  contract; any deliberate gap is recorded in the PR that introduces it.
- **OIDC instead of a shared bearer** for `/internal/*` (app-validated ID token).

---

## Private ingress

The private-ingress architecture serves an operator who needs the agent reachable **only on
their own network**. Each operator runs their **own** instance (`REPOS` scopes one instance to
one operator's repos — there is no shared multi-tenant service), and it works against **GitHub
Enterprise (self-hosted)**, **GitHub Enterprise Cloud**, and **GitHub public**. Cloud Run and
GCP-native cron are retained. Selecting this posture is config + infra only; the rollout work and
its status live in [`DEPLOYMENT.md`](../../DEPLOYMENT.md).

### Shape

A **self-hosted API gateway** on the operator's own network (GKE or a VM) is the single front
door, and Cloud Run is **private** — `ingress=internal-and-cloud-load-balancing`, fronted by an
**Internal Application Load Balancer** + serverless NEG. The gateway is a GCP-IAM caller: it
holds `roles/run.invoker` and presents a **Google OIDC ID token** (`aud` = the Cloud Run URL)
over mTLS to the private service, so `/internal/*` authenticates by OIDC (`INTERNAL_AUTH=oidc`)
rather than a shared bearer. The app continues to verify **HMAC** and the **`REPOS`** allowlist
itself, so those defenses hold end-to-end regardless of the edge.

```text
            operator network (enterprise VPC / personal net)
  ┌───────────────────────────────────────────────────────────────────────┐
  │   GitHub Enterprise (self-hosted) ─webhook(HMAC)─┐                      │
  │   CI runners (lint/coverage POST) ─HMAC──────────►│                     │
  │                                                   ▼                     │
  │                                            ┌─────────────┐  policies:   │
  │                                            │  self-hosted│  • HMAC verify (edge)
  │                                            │ API gateway │  • GitHub IP allowlist
  │                                            │ (operator   │  • rate-limit / replay
  │                                            │   network)  │  • size cap
  │                                            └──────┬──────┘              │
  │                                                   │  OIDC ID token (aud = Cloud Run URL) + mTLS
  │                                                   ▼                     │
  │                                   ┌────────────────────────────┐        │
  │                                   │ Internal Application LB     │        │
  │                                   │ (serverless NEG → Cloud Run)│        │
  └──────────────────────────────────┴──────────────┬─────────────┴────────┘
                                                     ▼  (Google-internal)
                                  ┌────────────────────────────────────────┐
                                  │ Cloud Run  ingress = internal-and-      │
                                  │            cloud-load-balancing         │
                                  │  IAM: roles/run.invoker = GW + Sched SA │
                                  │  app verifies HMAC + REPOS              │
                                  │  SESSION_BACKEND=firestore (scale-to-0) │
                                  └────────────────────────────────────────┘
                                                     ▲
   Cloud Scheduler ─OIDC(aud=Cloud Run)─► Internal ALB ─┘   /internal/cron/daily , /internal/sweep
```

`ingress=internal-and-cloud-load-balancing` admits the Internal ALB in the same VPC and nothing
from the public internet; the gateway is the only principal Cloud Run's IAM trusts. Cron and the
timeout sweep run through the same private path with a Cloud Scheduler OIDC token — GCP-native,
no public exposure, no shared secret.

### What each layer owns

| Layer | Responsibility |
|---|---|
| Self-hosted API gateway | edge TLS, **HMAC verify**, **GitHub IP allowlist**, **rate-limit + replay window**, request-size cap, **mint OIDC** + **mTLS** to backend, audit log |
| Internal ALB + serverless NEG | private L7 path gateway → Cloud Run |
| Cloud Run (private) | the agent; verifies HMAC + `REPOS` (defense in depth) |
| IAM | `roles/run.invoker` for the gateway SA + Scheduler SA; OIDC audience check |
| Cloud Scheduler | cron + sweep via OIDC through the Internal ALB |
| Secret Manager / gateway vault | HMAC secret, GitHub token, notifier URL |

### Edge policy at the gateway

The gateway is where the operator-owned edge policy lives: TLS termination, **HMAC
verification**, a **GitHub source-IP allowlist**, a **replay/timestamp window** (dedupe on
`X-GitHub-Delivery`), **rate-limiting**, and a request-size cap mirroring the app's 5 MiB body
limit. It mints the OIDC ID token, opens **mTLS** to the backend, and audit-logs every decision.
A self-hosted gateway on the operator network — rather than a managed public endpoint — is what
makes these webhook-shaped policies enforceable at the edge while the agent stays private; the
concrete product selection is recorded in [`DEPLOYMENT.md`](../../DEPLOYMENT.md).

### Serving each GitHub flavor

| Trigger | GHE self-hosted (operator net) | GHE Cloud / GitHub public |
|---|---|---|
| `check_run` resume (`/webhooks/github`) | gateway on operator net | gateway public listener, IP-allowlisted → private Cloud Run |
| lint/coverage kickoff (`/webhooks/{lint,coverage}`) | CI runners on operator net → gateway | CI → gateway (same listener) |
| daily digest (`/internal/cron/daily`) | Cloud Scheduler → OIDC → Internal ALB | same |
| timeout sweep (`/internal/sweep`) | Cloud Scheduler → OIDC → Internal ALB | same |

- **GHE self-hosted** — GitHub and the gateway both sit on the operator network, so ingress
  never leaves it.
- **GHE Cloud / GitHub public** — these deliver webhooks from the public internet, so the
  **gateway** terminates a single hardened public listener (GitHub IP allowlist + HMAC + replay
  window + rate-limit, optionally behind the operator's WAF/Cloud Armor or existing
  reverse-proxy/VPN edge) and forwards to the private Cloud Run.

Cron and the sweep are **always** GCP-internal regardless of GitHub flavor; only the
`check_run`/kickoff webhooks vary.

### Config

| Var | Meaning | Default |
|---|---|---|
| `INTERNAL_AUTH` | `bearer` \| `oidc` \| `both` for `/internal/*` | `bearer` |
| `OIDC_AUDIENCE` | expected `aud` (the Cloud Run service URL) | empty |
| `OIDC_ALLOWED_SA` | comma-list of allowed caller SA emails (gateway SA, Scheduler SA) | empty |

The defaults reproduce the public-URL behavior, so this posture is selected entirely through
config + infra. The agent logic (`fixflow`, sessions, park store, `REPOS`) is unchanged; the
architecture adds only an OIDC auth mode on `/internal/*` (optionally `/webhooks/*`), which
lands Go-first and mirrors per the parity contract.

---

## Other ports

The mental model, HTTP surface (`/webhooks/*` + `/internal/*`), env vars, and GCP steps
above apply to every port — they share one design and the `SESSION_BACKEND` switch. The
per-port difference is only the backend SDKs sitting behind the same interfaces:

| Port | sqlite session | firestore session | firestore park store |
|---|---|---|---|
| Go `go/` | adk `database` (gorm) | hand-rolled `session.Service` | custom on `cloud.google.com/go/firestore` |
| Python `python/` | adk `SqliteSessionService` | adk **native** `FirestoreSessionService` | custom on `google-cloud-firestore` |

Each port builds its own container (`cd <port> && make docker`, building that port's
`cmd/agent`). Any deliberate difference in a port's backend SDKs or coverage is recorded in
the PR that introduces it.
