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
                       webhook returns fast ─► enqueue on the execution transport
                                         │
                       TASKS_BACKEND = inprocess | cloudtasks
                          inprocess: background goroutine pool (local/default)
                          cloudtasks: Cloud Tasks ─bearer─► POST /internal/dispatch
                                         │              (runs the workflow IN-REQUEST)
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
| `POST /internal/dispatch` | Bearer | Cloud Tasks worker — run one queued workflow **in-request** (see [Execution transport](#execution-transport-webhook--dispatcher)) |

`/internal/*` are **disabled (404)** unless `INTERNAL_TOKEN` is set. HMAC verification is
skipped only when `GITHUB_WEBHOOK_SECRET` is empty (local dev).

### Execution transport (webhook → dispatcher)

A webhook handler must **return fast** (GitHub/your CI expects a prompt 2xx), but the
workflow it triggers is **multi-minute LLM compute**. On Cloud Run with the default
request-based billing, **CPU is throttled to near-zero once the response is sent**, so
running that compute in a post-202 background goroutine starves it and the instance can be
reclaimed mid-run. The fix: the webhook **enqueues** and the compute runs **inside a
request**, where CPU stays allocated up to the 60-minute request timeout.

`TASKS_BACKEND` selects how (a config switch, like `SESSION_BACKEND`):

| Backend | Use | Behavior |
|---|---|---|
| `inprocess` (default) | local dev | Background goroutine pool, drained on SIGTERM. Not durable — a reclaim loses in-flight work; on Cloud Run the compute is throttled after the 202. Reproduces the pre-transport behavior exactly. |
| `cloudtasks` | production | Each envelope is enqueued as a Cloud Tasks HTTP-target task → `POST /internal/dispatch`, which runs the workflow **synchronously, in-request**. The queue gives **durable retry with backoff** (a task survives a mid-run reclaim and is redelivered) and **rate limiting** (the queue's `max-concurrent-dispatches` replaces the in-process semaphore). |

Selecting `cloudtasks` is a production posture, so config validation **fails fast** unless
the queue coordinates, an **absolute `https://` `DISPATCH_URL`** (the task carries the
Bearer token to it — `http://` would leak it), `INTERNAL_TOKEN`, **and**
`GITHUB_WEBHOOK_SECRET` are all set (the webhook surface must be verified in prod, not just
warned about).

`/internal/dispatch` reuses the **same `INTERNAL_TOKEN` Bearer** as cron/sweep — the Cloud
Tasks task carries it as a header, so no new auth var and no OIDC. Retry classification
follows Cloud Tasks' retry-on-non-2xx contract: a transient dispatch error → `500` (the
queue retries); a poison body (undecodable / unknown kind) → `200` + log (dropped, not
retried). **Scale-to-zero is preserved** — no `min-instances` requirement. The fixers'
durable CI wait (Firestore park/resume) is unchanged and orthogonal: it offloads *waiting*,
this transport fixes *computing*. See `specs/20260626-workflow-execution-transport.md`.

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
| `INTERNAL_TOKEN` | **set** — from Secret Manager (else cron/sweep/dispatch are 404) |
| `TASKS_BACKEND` | `cloudtasks` (in-request workflow execution; `inprocess` only locally) |
| `TASKS_PROJECT` / `TASKS_LOCATION` / `TASKS_QUEUE` | the Cloud Tasks queue coordinates (project blank = `GOOGLE_CLOUD_PROJECT`) |
| `DISPATCH_URL` | full URL of `/internal/dispatch` the queue POSTs to (e.g. `https://<service>/internal/dispatch`) |
| `CI_TIMEOUT` | `90m` (default) — per-run CI wait before the timer/sweep frees a parked run |
| `GITHUB_APP_ID` / `GITHUB_APP_INSTALLATION_ID` | **set** — the production auth path (GitHub App installation tokens). One pinned installation per deployment (single org) |
| `GITHUB_APP_PRIVATE_KEY` | **set** — the App private-key PEM, from Secret Manager (a flattened `\n` is auto-restored) |
| `GITHUB_TOKEN`, notifier URLs | notifier URLs **set** — from Secret Manager. `GITHUB_TOKEN` is the local-dev fallback only; in App mode it is unused |
| `REPOS` | the kickoff allowlist — **required** in App mode (empty is rejected) |
| `FIRESTORE_PROJECT` / `FIRESTORE_COLLECTION` | blank = detect from ADC / default `automation_agent` |

### GitHub App setup (production auth)

Production authenticates as a **GitHub App** — short-lived (~1 h), repo-scoped
installation tokens instead of a long-lived PAT (bot identity on PRs, per-installation
rate limits, least-privilege, no personal-account coupling). **One App per deployment,
one org** (single-org topology — another org stands up its own deployment). Do this once,
before the GCP steps below; you end up with four values the service consumes.

1. **Create the App.** GitHub → your org → **Settings → Developer settings → GitHub Apps
   → New GitHub App**. Name it (e.g. `automation-agent-<org>`); homepage URL can be the repo.
2. **Permissions (least privilege).** Under **Repository permissions** set exactly these,
   leaving everything else **No access**:
   - **Contents** — Read & write (push fixer branches)
   - **Pull requests** — Read & write (open PRs, comment/review)
   - **Checks** — Read & write (create + receive `check_run`)
   - **Metadata** — Read-only (mandatory; auto-selected)

   The PR-review agent adds **no** new scopes beyond these.
3. **Webhook.** Tick **Active**. Set **Webhook URL** = `https://<service-host>/webhooks/github`
   and **Webhook secret** = the *same string* you will set as `GITHUB_WEBHOOK_SECRET`. This
   match is **mandatory** — if they differ, GitHub's `X-Hub-Signature-256` fails and every
   delivery 401s. (You can fill the URL in after Cloud Run has a hostname, but set the secret now.)
4. **Subscribe to events.** Check **Check run**. (The reviewer spec later adds **Pull request**.)
5. **Installation scope.** "Where can this app be installed?" → **Only on this account**.
6. **Create**, then note the numeric **App ID** from the App's *General* page → this is
   `GITHUB_APP_ID`.
7. **Generate a private key.** App page → **Private keys → Generate a private key**. A `.pem`
   downloads — it is the only copy; store it in a secret manager, never in git.
8. **Install the App.** App page → **Install App** → install on your org → choose **Only
   select repositories** and pick the repos the agent may act on (this physically scopes the
   tokens). The post-install URL ends `…/installations/<id>` → that `<id>` is
   `GITHUB_APP_INSTALLATION_ID`.

**Private-key delivery (set exactly one — both is a startup error):**
- **Cloud Run** → store the PEM in **Secret Manager**, mount as `GITHUB_APP_PRIVATE_KEY`
  (the literal multi-line PEM). A store that flattens newlines to literal `\n` is fine —
  the loader auto-restores them and validates the key parses (RSA) at startup.
- **Local dev** → `GITHUB_APP_PRIVATE_KEY_PATH=/path/to/key.pem` (sidesteps multi-line
  `.env` quoting).

**`REPOS` is required in App mode** (`owner/repo,owner/repo`) — an empty list is rejected so
the App never acts on every repo it can see. App mode engages automatically once
`GITHUB_APP_ID` + a key + `GITHUB_APP_INSTALLATION_ID` are all present; a partial set is a
startup error, never a silent PAT fallback.

**No App? (local dev)** Omit all `GITHUB_APP_*` vars and the service falls back to a PAT
(`GITHUB_TOKEN` / `gh auth token`) + SSH — behaviour identical to the pre-App path.

### GCP production setup (step by step)

1. **Firestore** — create a database in **Native mode** in your project. No schema/indexes
   to pre-create (single-field queries auto-index).
2. **Auth (ADC)** — give the Cloud Run service account `roles/datastore.user` (Firestore),
   `roles/cloudtasks.enqueuer` (create tasks on the queue), and, for Gemini-on-Vertex,
   `roles/aiplatform.user`. No keys needed; ADC is automatic on Cloud Run.
3. **GitHub App** — register it once per the [GitHub App setup](#github-app-setup-production-auth)
   section above. Have `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, the private-key PEM,
   and the shared `GITHUB_WEBHOOK_SECRET` ready before deploying.
4. **Build + deploy to Cloud Run.** `make docker` (or `docker build -t automation-agent
   go/`) builds `cmd/agent` only. Deploy with env: `SESSION_BACKEND=firestore`,
   `GOOGLE_CLOUD_PROJECT`, `LLM_PROVIDER=gemini` (+ `GOOGLE_GENAI_USE_VERTEXAI=TRUE`),
   `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY` (the PEM),
   `GITHUB_WEBHOOK_SECRET`, `INTERNAL_TOKEN`, `NOTIFY_PROVIDER` + webhook URL, `REPOS`.
   Store secrets (the App private key, webhook secret, internal token) in **Secret
   Manager** and mount them as env — not `.env`. (Omit the `GITHUB_APP_*` vars to fall
   back to a `GITHUB_TOKEN` PAT — the local-dev path, not recommended for production.)
   The App-level webhook from step 3 delivers `check_run` to `/webhooks/github`; there
   are **no per-repo webhooks** to configure. The lint/coverage kickoffs
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

6. **Cloud Tasks** — create one queue (`gcloud tasks queues create <name> --location=<region>`)
   and set `TASKS_BACKEND=cloudtasks`, `TASKS_LOCATION`, `TASKS_QUEUE`, and `DISPATCH_URL`
   (the service's `/internal/dispatch` URL). The queue POSTs each task with
   `Authorization: Bearer <INTERNAL_TOKEN>`, so the same secret that guards cron/sweep guards
   the worker. Tune the queue's `--max-concurrent-dispatches` (replaces the in-process
   concurrency cap) and set `--max-attempts` + a dead-letter as the poison backstop. Without
   this step the service still runs with `TASKS_BACKEND=inprocess`, but long workflows are
   throttled after the 202 on scale-to-zero — see [Execution
   transport](#execution-transport-webhook--dispatcher).

### Prod vs local stack

| Concern | Local | Prod (Cloud Run) |
|---|---|---|
| Compute | `make run`, single process | Cloud Run + `SESSION_BACKEND=firestore` (scale-to-zero); or `min-instances=1` / a GCE VM for the in-memory mode |
| LLM | Ollama (default) | `LLM_PROVIDER=gemini` (Vertex) unless a GPU VM runs Ollama |
| Session + park store | `memory` / `sqlite` | `firestore` (durable; a restart resumes in-flight runs) |
| Secrets | `.env` | Secret Manager mounted as env |
| Scheduler | Cloud Scheduler → `/internal/cron/daily` + `/internal/sweep` (Bearer) | same — Cloud Scheduler is the only trigger; no in-process cron |
| Execution transport | `TASKS_BACKEND=inprocess` — background goroutine pool | `TASKS_BACKEND=cloudtasks` — Cloud Tasks → `/internal/dispatch`, in-request (CPU stays allocated, durable retry) |
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
