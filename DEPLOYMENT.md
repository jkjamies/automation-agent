# Deployment & operations

Practical guide to running automation-agent locally and on GCP, the env vars and the
HTTP hooks, and the known TODOs. Written to be followed without deep GCP expertise.

> Scope: the Go service (`go/`). The Python / TS / Kotlin ports mirror this design but are
> not yet updated (see **TODO: parity**).

## Mental model

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

The fix loop opens a PR, **suspends** waiting for CI, and **resumes** when GitHub posts the
`check_run` result. With a durable backend (sqlite locally, **firestore** in prod) a
restart no longer strands in-flight runs, which is what lets Cloud Run scale toward zero.

## HTTP hooks (endpoints)

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

### Why a shared bearer for `/internal/*` (and not OIDC yet)

Some auth is genuinely required: Cloud Run serves a **public** URL, and the service *must*
be public because **GitHub webhooks can't authenticate to Google IAM** (they only sign with
HMAC). So `/webhooks/*` forces the whole service public, which leaves `/internal/*` reachable
by anyone who knows the URL.

The GCP best practice would be **OIDC** (Cloud Scheduler mints an ID token; the platform or
the app verifies it) — but full IAM-OIDC wants a *private* service, which conflicts with the
public webhook surface (you'd have to split into two services or front it with a load
balancer + IAP). So we use a **shared bearer token (`INTERNAL_TOKEN`)** as the pragmatic
guard on the single public service. Blast radius is modest even so: `/internal/sweep` only
resolves runs *already past `CI_TIMEOUT`* (it can't touch fresh runs); the cron endpoints
only trigger digests. **Decision: bearer now, OIDC later** — the cleanest upgrade is
*app-validated OIDC* (verify the Google ID token in the handler, audience-checked), which
keeps a single service and drops the shared secret. Tracked under TODO.

## Environment variables

| Var | Default | Notes |
|---|---|---|
| **LLM** | | |
| `LLM_PROVIDER` | `ollama` | `ollama` \| `gemini` |
| `OLLAMA_HOST` / `OLLAMA_MODEL` / `OLLAMA_CODE_MODEL` | localhost / `gemma4:12b` / `gemma4:26b` | local models |
| `GEMINI_MODEL` / `GEMINI_CODE_MODEL` | — | used when `LLM_PROVIDER=gemini` |
| `GOOGLE_GENAI_USE_VERTEXAI`, `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`, `GOOGLE_API_KEY` | — | **SDK-owned** (not in Config). Vertex: `=TRUE`+project+location+ADC. AI Studio: `=FALSE`+`GOOGLE_API_KEY`. |
| **Sessions (durable suspend/resume)** | | |
| `SESSION_BACKEND` | `memory` | `memory` (tests/ephemeral) \| `sqlite` (durable local) \| `firestore` (cloud) |
| `SQLITE_DSN` | `file:automation-agent.db?_pragma=busy_timeout(5000)` | used when `=sqlite` |
| `FIRESTORE_PROJECT` | — | blank = detect from ADC / `GOOGLE_CLOUD_PROJECT` |
| `FIRESTORE_COLLECTION` | `automation_agent` | collection-name prefix (`_sessions`, `_app_state`, `_user_state`, `_parked_runs`) |
| **Ingress / auth** | | |
| `GITHUB_WEBHOOK_SECRET` | — | HMAC for `/webhooks/*`; **set in prod** |
| `INTERNAL_TOKEN` | — | Bearer for `/internal/*`; **set in prod** (else cron/sweep are 404) |
| **GitHub** | | |
| `GITHUB_TOKEN` | — | PR create/label/compare |
| `REPOS` | — | `owner/repo,owner/repo2` kickoff allowlist (empty = no restriction) |
| **Notify** | | |
| `NOTIFY_PROVIDER` / `SLACK_WEBHOOK_URL` / `TEAMS_WEBHOOK_URL` | `slack` | where digests + fix results post |
| **Server / schedule** | | |
| `PORT` | `8080` | |
| `CRON_DAILY` / `CRON_WEEKLY` | `0 9 * * *` / `0 9 * * 1` | **in-process** scheduler (see caution below) |
| `MAX_ITERATIONS` | `3` | fix attempts before "needs review" |
| `CI_TIMEOUT` | `90m` | how long a parked run waits before the sweep/timer frees it |

See `.env.example` for a copy-paste starting point.

## Local development

```bash
# zero cloud: in-memory or durable-local
cd go
SESSION_BACKEND=memory make run      # ephemeral (default)
SESSION_BACKEND=sqlite make run      # parked runs survive a restart, stored in a local file

make ci                              # vet + arch + tests + coverage (memory + sqlite)
```

Testing the **firestore** code locally needs the emulator (Java 17+):

```bash
gcloud components install cloud-firestore-emulator   # one-time
gcloud beta emulators firestore start --host-port=localhost:8085 &
FIRESTORE_EMULATOR_HOST=localhost:8085 GOOGLE_CLOUD_PROJECT=test make cover-firestore
```

`make ci` excludes the emulator-only `*_firestore.go` from its coverage gate; the firestore
code is validated by `make cover-firestore` against adk's own conformance suites.

## GCP production setup (step by step)

1. **Firestore** — create a database in **Native mode** in your project. No schema/indexes
   to pre-create (single-field queries auto-index).
2. **Auth (ADC)** — give the Cloud Run service account `roles/datastore.user` (Firestore)
   and, for Gemini-on-Vertex, `roles/aiplatform.user`. No keys needed; ADC is automatic on
   Cloud Run.
3. **Deploy to Cloud Run** with env: `SESSION_BACKEND=firestore`, `GOOGLE_CLOUD_PROJECT`,
   `LLM_PROVIDER=gemini` (+ `GOOGLE_GENAI_USE_VERTEXAI=TRUE`), `GITHUB_TOKEN`,
   `GITHUB_WEBHOOK_SECRET`, `INTERNAL_TOKEN`, `NOTIFY_PROVIDER` + webhook URL, `REPOS`.
   Store secrets in Secret Manager and mount them as env.
4. **GitHub webhook** — in the repo settings add a webhook → `https://<service>/webhooks/github`,
   content-type `application/json`, secret = `GITHUB_WEBHOOK_SECRET`, events: *Check runs*.
   The lint/coverage kickoffs (`/webhooks/{lint,coverage}`) are POSTed by your CI with the
   same secret in `X-Hub-Signature-256`.
5. **Cloud Scheduler** — three jobs, each an HTTP POST with header
   `Authorization: Bearer <INTERNAL_TOKEN>` (or use an OIDC token + tighten the handler):
   | Job | Target | Schedule (cron) |
   |---|---|---|
   | daily digest | `POST /internal/cron/daily` | `0 9 * * *` |
   | weekly digest | `POST /internal/cron/weekly` | `0 9 * * 1` |
   | timeout sweep | `POST /internal/sweep` | e.g. `*/15 * * * *` |

> **Caution — don't double-fire the digests.** The app *also* has an in-process cron
> (`CRON_DAILY`/`CRON_WEEKLY`). If you use Cloud Scheduler (the scale-to-zero path), the
> in-process scheduler will *also* fire whenever an instance is warm → duplicate digests.
> Until there's a flag to disable it (see TODO), pick **one**: either keep `min-instances=1`
> and use the in-process cron (no Scheduler cron jobs), **or** use Cloud Scheduler and treat
> the in-process cron as redundant.

## TODO (not yet implemented)

- [ ] **Orphan-session GC.** The `/internal/sweep` business timeout only sees runs in
      `parked_runs`. A session created but **never parked** (crash between session-create and
      park) has no park record and leaks (firestore especially). Add a cleanup hook that
      deletes sessions whose **last-updated** (`updated_at`) is older than a stale threshold
      (≈ `CI_TIMEOUT × MAX_ITERATIONS` + margin, ~6–24h). Works for firestore + sqlite; can
      ride `/internal/sweep` or a Firestore native TTL policy on the `_sessions` collection.
- [ ] **Disable the in-process scheduler** via a flag (e.g. `SCHEDULER=external`) so Cloud
      Scheduler + `min-instances=0` is safe without duplicate digests (see caution above).
- [ ] **Terraform/IaC** for Firestore + Cloud Run + Cloud Scheduler + Secret Manager; today
      the steps above are manual.
- [ ] **CI runs the Firestore emulator** so `*_firestore.go` folds back into measured coverage.
- [ ] **Parity:** mirror the durable-session design (Phases A–E) to Python / TS / Kotlin.
- [ ] **OIDC instead of a shared bearer** for `/internal/*` (tighter than `INTERNAL_TOKEN`).
