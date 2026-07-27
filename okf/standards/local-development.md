---
type: Standard
title: Local development
description: How to run the service locally in every mode — prerequisites, run targets, backend switches, and the full environment-variable reference.
tags: [local-development, configuration, environment]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Local development

How to run the service on your machine — prerequisites, configuration, every run mode,
and how the local stack differs from prod. Source of truth; read it and you can run the
agent locally without asking anyone.

> Related: [Testing](/standards/testing.md) (running tests) ·
> [Deployment](/standards/deployment.md) (cloud/GCP) ·
> [CI Integration](/standards/ci-integration.md) (driving the lint/coverage fixers).

---

## Go (`go/`) — reference

**Run everything from the `go/` directory.** Targets live in `go/Makefile`.

### Prerequisites

- **Go 1.26**.
- **[Ollama](https://ollama.com)** running locally with a Gemma model (the default local
  LLM). Pull a model and check what the server actually has:
  ```bash
  ollama pull gemma4:12b        # the default OLLAMA_MODEL (gemma4:26b for code changes)
  cd go && make ollama-check    # reachability + the list of models pulled
  ```
  **Model tags are a moving target.** A family gets a new generation, a size is renamed, a tag
  is withdrawn — so treat the defaults above as a starting point and let `make ollama-check`
  tell you what your server has. If the configured tag is not among them, `make run` says so in
  one warning at startup, naming the tag and the `ollama pull` that fixes it (see
  [LLM selection](#llm-selection--llm_provider)), rather than leaving you to discover it on the
  first webhook.
  (Or skip Ollama and point at Vertex/AI-Studio Gemini — see [LLM selection](#llm-selection--llm_provider).)
- A **`.env`** file — copy the starting point and edit:
  ```bash
  cp .env.example .env          # repo root
  ```
  All run modes load `.env` automatically (godotenv; a no-op if absent).
- For the **Firestore** backend locally: the Cloud Firestore emulator (Java 17+) — see
  [Testing § Firestore-backed tests](/standards/testing.md#firestore-backed-tests-emulator) and
  [Deployment](/standards/deployment.md).

### Run modes

```bash
cd go
make run                          # the service: webhooks + /internal cron hooks (cmd/agent), SESSION_BACKEND=memory
SESSION_BACKEND=sqlite make run   # durable local: parked runs survive a restart (a local .db file)
make playground                   # local ADK web UI + CLI at http://localhost:8080 (cmd/playground, dev only)
make ci                           # the full local gate (tidy-check + vet + lint + arch + test + cover)
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

> The two-tier split is deliberate: summarization/triage uses the smaller base model; code
> reasoning and edits use the larger code model. The sizes in the defaults are a starting
> point, not a contract — pick whatever your hardware runs well.

**Startup verification (ollama only).** On boot the service lists the server's models and
confirms both configured tags are present. It is **advisory** — configuring the deployment is
yours to get right, and this never blocks the boot. A missing tag (or an unreachable server) is
one warning naming the tag, the `ollama pull` that fixes it, and the models the server does
have; without it the first sign of a stale tag is an opaque failure on the first generation,
after a webhook has been accepted, a task dispatched, and a repository cloned. Nothing is checked
under `LLM_PROVIDER=gemini`.

### Environment variables (full reference)

Only `internal/config` reads the environment. `Validate()` enforces the enums and ranges.
`.env.example` is the copy-paste starting point.

| Var | Default | Notes |
|---|---|---|
| **LLM** | | |
| `LLM_PROVIDER` | `ollama` | `ollama` \| `gemini` |
| `OLLAMA_HOST` | `http://localhost:11434` | local Ollama server |
| `OLLAMA_NUM_CTX` | `32768` | context window per Ollama call; also the budget the reviewer's size gate derives from. Bounded (1 … 2^24) because the derived byte cap would otherwise overflow, and a non-positive cap turns the size gate off rather than failing |
| `LLM_MAX_CONCURRENT` | `2` (ollama) / `8` (gemini) | max model calls in flight process-wide |
| `FIX_MAX_FILES` | `50` | max files one fix attempt edits; the rest are reported, not silently dropped |
| `OLLAMA_MODEL` | `gemma4:12b` | triage / explore / summary; verified at startup |
| `OLLAMA_CODE_MODEL` | `gemma4:26b` | code changes; blank → `OLLAMA_MODEL`; verified at startup |
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
| **Observability (tracing)** | | |
| `OTEL_TRACES_EXPORTER` | `none` | `none` (off) \| `console` (stdout) \| `otlp` \| `gcp` (Cloud Trace via ADC). The playground defaults to `console` when this is unset. |
| `OTEL_SERVICE_NAME` | `automation-agent` | resource `service.name` on every span |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP/HTTP endpoint; **required** for `otlp` |
| `OTEL_EXPORTER_OTLP_HEADERS` | — | OTLP headers as `k=v,...` (often a vendor API key — secret, masked in the config log view) |
| `OTEL_TRACES_SAMPLER` | `parentbased_always_on` | standard sampler value |
| `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` | `false` | opt-in capture of prompt/response bodies (sensitive); the standard GenAI-semconv var the framework reads natively |

### What each feature needs to actually do something

- **Daily summary** needs `REPOS` **and** a notifier (`SLACK_WEBHOOK_URL` or
  `TEAMS_WEBHOOK_URL`). Without a notifier it logs "disabled" and runs webhooks-only.
- **Lint-fixer / coverage-fixer** need a `GITHUB_TOKEN` with repo scope to open and label
  PRs (the REST API), and each target repo needs the `agent-lint-verify` /
  `agent-coverage-verify` workflow plus a `check_run` webhook back to the agent — see
  [CI Integration](/standards/ci-integration.md). To push over SSH locally set `GIT_TRANSPORT=ssh`
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

See [CI Integration](/standards/ci-integration.md) for the full contract, HMAC signing, the
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
