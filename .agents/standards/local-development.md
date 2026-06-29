# Local development

How to run the service on your machine — prerequisites, configuration, every run mode,
and how the local stack differs from prod. Source of truth; read it and you can run the
agent locally without asking anyone.

> **Scope:** the detailed walkthrough below uses the **Go** reference (`go/`); the same run
> modes and env vars apply to every port — run them from that port's directory (see
> [Other ports](#other-ports)).
>
> Related: [`testing.md`](testing.md) (running tests) · [`deployment.md`](deployment.md)
> (cloud/GCP) · [`ci-integration.md`](ci-integration.md) (driving the lint/coverage fixers).

---

## Go (`go/`) — reference

**Run everything from the `go/` directory.** Targets live in `go/Makefile`.

### Prerequisites

- **Go 1.26**.
- **[Ollama](https://ollama.com)** running locally with a Gemma model (the default local
  LLM). Pull a model and check it's reachable:
  ```bash
  ollama pull gemma3            # the project defaults to gemma4:* model names
  cd go && make ollama-check    # curls $OLLAMA_HOST/api/tags
  ```
  (Or skip Ollama and point at Vertex/AI-Studio Gemini — see [LLM selection](#llm-selection--llm_provider).)
- A **`.env`** file — copy the starting point and edit:
  ```bash
  cp .env.example .env          # repo root
  ```
  All run modes load `.env` automatically (godotenv; a no-op if absent).
- For the **Firestore** backend locally: the Cloud Firestore emulator (Java 17+) — see
  [`testing.md`](testing.md#firestore-backed-tests-emulator) and
  [`deployment.md`](deployment.md).

### Run modes

```bash
cd go
make run                          # the service: webhooks + /internal cron hooks (cmd/agent), SESSION_BACKEND=memory
SESSION_BACKEND=sqlite make run   # durable local: parked runs survive a restart (a local .db file)
make playground                   # local ADK web UI + CLI at http://localhost:8080 (cmd/playground, dev only)
make ci                           # the full local gate (tidy + vet + arch + test + cover)
```

- **`make run`** → `go run ./cmd/agent`. Loads `.env`, builds the LLM + session service +
  park store, wires the agents, starts the HTTP server on `PORT` (default `8080`), and drains
  gracefully on SIGINT/SIGTERM. There is no in-process cron — the daily digest is triggered by
  POSTing `/internal/cron/daily` (Cloud Scheduler in prod; `curl` + Bearer token locally).
- **`make playground`** → `go run ./cmd/playground web api webui`. A **dev-only** binary
  (never deployed) for poking the configured model. `go run ./cmd/playground console`
  gives an interactive CLI instead.

### Choosing the local stack

Two independent switches decide the local stack. Both default to the lightest option, so
a bare `make run` needs no cloud at all.

#### Session / park-store backend — `SESSION_BACKEND`

Selects where the suspend/resume session **and** the park record (`prKey → session,
attempts, params`) live:

| Value | Meaning locally |
|---|---|
| `memory` (default) | In-process. A restart **drops** parked runs. Fine for most dev. |
| `sqlite` | Durable local file (`SQLITE_DSN`, default `file:automation-agent.db?_pragma=busy_timeout(5000)`). Parked runs survive a restart. |
| `firestore` | Cloud — needs the emulator locally (`FIRESTORE_EMULATOR_HOST`) or a real project. Mainly for testing the cloud path. |

#### LLM selection — `LLM_PROVIDER`

| Value | Setup |
|---|---|
| `ollama` (default) | Local models. `OLLAMA_HOST` (default `http://localhost:11434`), `OLLAMA_MODEL` (`gemma4:12b`, used for triage/explore/summary), `OLLAMA_CODE_MODEL` (`gemma4:26b`, for code changes; falls back to `OLLAMA_MODEL`). |
| `gemini` | Vertex or AI Studio. Set `GEMINI_MODEL` (+ `GEMINI_CODE_MODEL`), and the SDK-owned vars: Vertex → `GOOGLE_GENAI_USE_VERTEXAI=TRUE` + `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_LOCATION` + ADC; AI Studio → `GOOGLE_GENAI_USE_VERTEXAI=FALSE` + `GOOGLE_API_KEY`. |

> The 12b/26b split is deliberate: summarization/triage uses the smaller base model;
> code reasoning and edits use the larger code model.

### Environment variables (full reference)

Only `internal/config` reads the environment. `Validate()` enforces the enums and ranges.
`.env.example` is the copy-paste starting point.

| Var | Default | Notes |
|---|---|---|
| **LLM** | | |
| `LLM_PROVIDER` | `ollama` | `ollama` \| `gemini` |
| `OLLAMA_HOST` | `http://localhost:11434` | local Ollama server |
| `OLLAMA_MODEL` | `gemma4:12b` | triage / explore / summary |
| `OLLAMA_CODE_MODEL` | `gemma4:26b` | code changes; blank → `OLLAMA_MODEL` |
| `GEMINI_MODEL` / `GEMINI_CODE_MODEL` | — | used when `LLM_PROVIDER=gemini`; code blank → base |
| `GOOGLE_GENAI_USE_VERTEXAI`, `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`, `GOOGLE_API_KEY` | — | **SDK-owned** (not in `Config`). Vertex: `=TRUE`+project+location+ADC. AI Studio: `=FALSE`+`GOOGLE_API_KEY`. |
| **Sessions (durable suspend/resume)** | | |
| `SESSION_BACKEND` | `memory` | `memory` \| `sqlite` \| `firestore` |
| `SQLITE_DSN` | `file:automation-agent.db?_pragma=busy_timeout(5000)` | used when `=sqlite` |
| `FIRESTORE_PROJECT` | — | blank = detect from ADC / `GOOGLE_CLOUD_PROJECT` |
| `FIRESTORE_COLLECTION` | `automation_agent` | prefix for `_sessions`, `_app_state`, `_user_state`, `_parked_runs` |
| **Ingress / auth** | | |
| `GITHUB_WEBHOOK_SECRET` | — | HMAC for `/webhooks/*`; **blank locally = verification skipped (dev only)** |
| `INTERNAL_TOKEN` | — | Bearer for `/internal/*` (cron, sweep, dispatch); blank = those routes are 404 |
| **Execution transport (webhook → dispatcher)** | | |
| `TASKS_BACKEND` | `inprocess` | `inprocess` (in-process background worker pool — local) \| `cloudtasks` (Cloud Tasks → `/internal/dispatch`, in-request — prod) |
| `TASKS_PROJECT` | `GOOGLE_CLOUD_PROJECT` | GCP project owning the queue; `cloudtasks` only |
| `TASKS_LOCATION` | — | queue region (e.g. `us-central1`); **required** for `cloudtasks` |
| `TASKS_QUEUE` | — | Cloud Tasks queue name; **required** for `cloudtasks` |
| `DISPATCH_URL` | — | full URL of `/internal/dispatch` the queue POSTs to (must end in `/internal/dispatch`); **required** for `cloudtasks` |
| `TASKS_DISPATCH_DEADLINE` | `30m` | explicit per-task dispatch deadline; range `15s`..`30m` (Cloud Tasks max), `cloudtasks` only |
| **GitHub** | | |
| `GITHUB_TOKEN` | `GH_TOKEN`, then `gh auth token` | PR create/label/compare (repo scope); blank reuses your local `gh` login. Also the `https` git transport. |
| `GIT_TRANSPORT` | `https` | `https` (token) \| `ssh` (clone/push over ssh-agent/keys). **SSH only covers git transport — PR ops still need a token / `gh` login.** |
| `GIT_SSH_KEY` | — | `GIT_TRANSPORT=ssh`: explicit key path; blank = ssh-agent then `~/.ssh/id_*` |
| `REPOS` | — | `owner/repo,owner/repo2` kickoff allowlist (empty = no restriction) |
| **Notify** | | |
| `NOTIFY_PROVIDER` | `slack` | `slack` \| `teams` |
| `SLACK_WEBHOOK_URL` / `TEAMS_WEBHOOK_URL` | — | required for the chosen provider |
| **Server** | | |
| `PORT` | `8080` | HTTP port |
| `MAX_ITERATIONS` | `3` | fix attempts before "needs review" |
| `CI_TIMEOUT` | `90m` | how long a parked run waits before the sweep/timer frees it |

### What each feature needs to actually do something

- **Daily summary** needs `REPOS` **and** a notifier (`SLACK_WEBHOOK_URL` or
  `TEAMS_WEBHOOK_URL`). Without a notifier it logs "disabled" and runs webhooks-only.
- **Lint-fixer / coverage-fixer** need a `GITHUB_TOKEN` with repo scope to open and label
  PRs (the REST API), and each target repo needs the `agent-lint-verify` /
  `agent-coverage-verify` workflow plus a `check_run` webhook back to the agent — see
  [`ci-integration.md`](ci-integration.md). To push over SSH locally set `GIT_TRANSPORT=ssh`
  (uses your ssh-agent/keys for clone+push) — but you still need the token/`gh` login above
  for the PR operations, since SSH does not authenticate the REST API.

### Exercising webhooks locally

The kickoff endpoints accept the same envelope CI sends. With `GITHUB_WEBHOOK_SECRET`
unset locally, no HMAC header is required:

```bash
curl -sf -X POST http://localhost:8080/webhooks/lint \
  -H 'content-type: application/json' \
  -d '{"repo":"owner/name","base":"main","report":"<your linter output>"}'
```

See [`ci-integration.md`](ci-integration.md) for the full contract, HMAC signing, the
coverage endpoint, and the resume (`check_run`) side. The `/internal/*` cron + sweep
routes return 404 unless `INTERNAL_TOKEN` is set.

### Local container

```bash
cd go
make docker                                   # docker build -t automation-agent .  (cmd/agent only)
docker run --rm -p 8080:8080 --env-file ../.env \
  -e OLLAMA_HOST=http://host.docker.internal:11434 \
  automation-agent
```

The image builds **only** `cmd/agent` (the playground is never containerized). Point
`OLLAMA_HOST` at the host's Ollama, or set `LLM_PROVIDER=gemini` to use Vertex.

---

## Other ports

Every port has the same run modes and `make` targets as Go, run from its own directory.

### Python (`python/`)

Prerequisites: Python 3.13 + [uv](https://github.com/astral-sh/uv), an Ollama with a Gemma
model (or `LLM_PROVIDER=gemini`), and a `.env` (copy `python/.env.example`).

```bash
cd python
make build                        # uv sync
make run                          # webhooks + /internal cron hooks (cmd/agent), SESSION_BACKEND=memory
SESSION_BACKEND=sqlite make run   # durable local: parked runs survive a restart
make playground                   # local ADK web UI
```

`SESSION_BACKEND` selects the same `memory | sqlite | firestore` backends as Go. Python's
`SQLITE_DSN` is a **plain file path** (default `automation-agent.db`) — adk's
`SqliteSessionService` takes a path, not Go's `file:…?_pragma=` DSN — and `firestore` uses
adk's native `FirestoreSessionService` plus a custom park store on `google-cloud-firestore`.
The `/internal/*` ingress + `INTERNAL_TOKEN` behave identically.

### TypeScript (`javascript/`) and Kotlin (`kotlin/`)

The same `make run` / `make playground` / `make ci` targets apply to `javascript/`.
`kotlin/` uses the Gradle wrapper (`./gradlew build`, `./gradlew run`, `./gradlew koverVerify`).
Any deliberate divergence in a port's run flags or backend SDKs is recorded in
the PR that introduces it.
