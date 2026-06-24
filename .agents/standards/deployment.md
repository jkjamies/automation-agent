# Deployment

The canonical deployment + operations reference. How the service is structured for the
cloud, the HTTP surface, and the step-by-step GCP setup. This is the **source of truth**;
the repo-root [`DEPLOYMENT.md`](../../DEPLOYMENT.md) is a thin status/checklist pointer
back here (no environment is stood up yet — the repo is code only).

> **Scope:** the GCP walkthrough below uses the **Go** reference (`go/`) as the worked
> example; the same design, HTTP surface, and `SESSION_BACKEND` switch apply to every port
> (see [Other ports](#other-ports) for the per-port backend stacks). Per-port drift:
> `specs/parity-status.md`.
>
> Related: [`local-development.md`](local-development.md) (run it on your machine) ·
> [`testing.md`](testing.md) (Firestore emulator) · [`ci-integration.md`](ci-integration.md)
> (driving the fixers from CI) · `.agents/standards/architecture-design.md` §8, §13 (design).

---

## Go (`go/`) — reference

### Mental model

```
 GitHub repo ──webhook(HMAC)──► POST /webhooks/{lint,coverage,github}
 Cloud Scheduler ─bearer─►       POST /internal/cron/{daily,weekly}   (digests)
 Cloud Scheduler ─bearer─►       POST /internal/sweep                 (timeout catch-all)
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
| `POST /internal/cron/weekly` | Bearer | fire the weekly digest |
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
[TODO](#todo--not-yet-implemented).

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
5. **Cloud Scheduler** — three jobs, each an HTTP POST with header
   `Authorization: Bearer <INTERNAL_TOKEN>` (or an OIDC token + a tightened handler):

   | Job | Target | Schedule (cron) |
   |---|---|---|
   | daily digest | `POST /internal/cron/daily` | `0 9 * * *` |
   | weekly digest | `POST /internal/cron/weekly` | `0 9 * * 1` |
   | timeout sweep | `POST /internal/sweep` | e.g. `*/15 * * * *` |

> **Caution — don't double-fire the digests.** The app *also* runs an in-process cron
> (`CRON_DAILY`/`CRON_WEEKLY`). If you use Cloud Scheduler (the scale-to-zero path), the
> in-process scheduler will *also* fire whenever an instance is warm → duplicate digests.
> Until there's a flag to disable it (see TODO), pick **one**: either keep
> `min-instances=1` and use the in-process cron (no Scheduler cron jobs), **or** use Cloud
> Scheduler and treat the in-process cron as redundant.

### Prod vs local stack

| Concern | Local | Prod (Cloud Run) |
|---|---|---|
| Compute | `make run`, single process | Cloud Run + `SESSION_BACKEND=firestore` (scale-to-zero); or `min-instances=1` / a GCE VM for the in-memory mode |
| LLM | Ollama (default) | `LLM_PROVIDER=gemini` (Vertex) unless a GPU VM runs Ollama |
| Session + park store | `memory` / `sqlite` | `firestore` (durable; a restart resumes in-flight runs) |
| Secrets | `.env` | Secret Manager mounted as env |
| Scheduler | in-process cron | Cloud Scheduler → `/internal/cron/*` + `/internal/sweep` (Bearer) — disable the in-process cron, see caution |
| Timeout safety | in-process per-run timer | the timer **and** the durable `/internal/sweep` catch-all |
| HA / scale-out | n/a | `firestore` is a shared store with atomic single-winner claims, so replicas can in principle share it; not exercised yet |

### CI/CD

GitHub Actions builds/pushes the image and deploys to Cloud Run. (IaC is a
[TODO](#todo--not-yet-implemented); the setup steps above are manual today.)

### TODO (not yet implemented)

- [ ] **Orphan-session GC.** The `/internal/sweep` business timeout only sees runs in
      `parked_runs`. A session created but **never parked** (a crash between session-create
      and park) has no park record and leaks (firestore especially). Add a cleanup hook
      that deletes sessions whose `updated_at` is older than a stale threshold
      (≈ `CI_TIMEOUT × MAX_ITERATIONS` + margin, ~6–24h). Works for firestore + sqlite; can
      ride `/internal/sweep` or a Firestore native TTL policy on `_sessions`.
- [ ] **Disable the in-process scheduler** via a flag (e.g. `SCHEDULER=external`) so Cloud
      Scheduler + `min-instances=0` is safe without duplicate digests (see caution).
- [ ] **Terraform/IaC** for Firestore + Cloud Run + Cloud Scheduler + Secret Manager.
- [ ] **CI runs the Firestore emulator** so `*_firestore.go` folds back into measured
      coverage (see [`testing.md`](testing.md)).
- [ ] **Cross-port parity:** keep the ports in lockstep on the durable-session design;
      current per-port drift is tracked in `specs/parity-status.md`.
- [ ] **OIDC instead of a shared bearer** for `/internal/*` (app-validated ID token).

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
`cmd/agent`). Where a port's backend SDKs or coverage differ, `specs/parity-status.md` is
the record.
