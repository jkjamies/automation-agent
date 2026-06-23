# automation-agent

A lightweight, long-running Go service that ingests events from many sources
(cron today; GitHub/Jira/Confluence/human later), routes every ingest through a
**root agent**, and runs three workflow agents:

- **Summary** — daily/weekly digest of the last 24h of commits across N repos,
  posted to Slack or Teams.
- **Lint-fixer** — consumes an agnostic lint payload, opens a PR with a fix, and
  loops (max 3) on CI feedback before posting a result. Suspend/resume rides on
  ADK long-running tools plus a **pluggable durable backend** (`SESSION_BACKEND` =
  `memory` | `sqlite` | `firestore`): with a durable backend a process restart
  **resumes cleanly** instead of stranding in-flight runs, and terminal results post
  a status-aware summary (what changed on the PR + the targeted findings).
- **Coverage-fixer** — consumes an agnostic coverage report (JaCoCo, lcov, `go cover`,
  …) and opens a PR adding tests for *meaningful* uncovered logic, with the same CI
  loop. Shares the `fixflow` engine with the lint-fixer.

Built on the [Agent Development Kit for Go](https://github.com/google/adk-go),
local-first on **Ollama + Gemma**, with a config switch to **Gemini/Vertex** for
cloud deployment.

> **Design doc:** [`.agents/standards/architecture-design.md`](.agents/standards/architecture-design.md) is the source of
> truth for the architecture and decisions.

## Ports (Go · Kotlin · Python · TypeScript)

The Go implementation in [`go/`](go/) is the canonical reference. It is mirrored by
sibling ports that must **all stay 1:1 in functionality** — same structure, public
surface, config, and external contracts. Every language is held to the same parity
contract; a behavior change lands in Go first and is mirrored into every existing port in
the same change (see [`.agents/standards/language-parity.md`](.agents/standards/language-parity.md)):

- **Kotlin** — [`kotlin/`](kotlin/), built on [ADK for Kotlin](https://github.com/google/adk-kotlin)
  (`com.google.adk:google-adk-kotlin-core:0.2.0`). A functional 1:1 port (`gradle build`
  green).
- **Python** — [`python/`](python/), built on `google-adk` from PyPI. A functional 1:1
  port (`make ci` green).
- **TypeScript** — [`javascript/`](javascript/), built on the official
  [ADK for JavaScript](https://github.com/google/adk-js) (`@google/adk`). A functional 1:1
  port (`make ci` green).

Each port uses its language's **native ADK**, so parity is functional, not version-matched.

## Quick start

```bash
cp .env.example .env      # then edit
make help                 # list all targets
make ci                   # tidy + vet + arch + test + coverage gate
make run                  # run the service
make playground           # local ADK web UI at http://localhost:8080 (dev only)
```

How-to guides (Go reference; other ports pending parity):
[`local-development.md`](.agents/standards/local-development.md) (run modes, env vars,
container), [`testing.md`](.agents/standards/testing.md) (every test kind + the Firestore
emulator), [`deployment.md`](.agents/standards/deployment.md) (cloud architecture + GCP
setup — source of truth), and [`ci-integration.md`](.agents/standards/ci-integration.md)
(how CI drives the fixers). [`DEPLOYMENT.md`](DEPLOYMENT.md) is the short status/checklist.

### Durable sessions (Go)

The fix loop's suspend/resume state is stored behind one `SESSION_BACKEND` switch —
`memory` (default, zero-dependency), `sqlite` (durable local file), or `firestore` (cloud,
scale-to-zero). Cloud Scheduler can drive the digests and the timeout sweep via
`POST /internal/cron/{daily,weekly}` and `POST /internal/sweep` (Bearer-auth'd with
`INTERNAL_TOKEN`). This landed in **Go first**; the Python / TS / Kotlin ports still use the
in-memory design and are pending parity (see [`DEPLOYMENT.md`](DEPLOYMENT.md) TODOs).

## Status & TODO

Phases 1–5, plus the **durable-sessions migration** (spike + Phases A–D: `SESSION_BACKEND`
switch, the `ParkStore` seam replacing the in-memory registry, Firestore session/park
backends, status-aware summaries, and Cloud Scheduler `/internal` ingress) are implemented
**in Go** and `make ci` is green (≥80% coverage; Firestore validated against the emulator).
The core service runs locally; the LLM steps are verified against real Gemma. Remaining
work to reach a fully production-validated system:

- [ ] **Add the `agent-lint-verify` GitHub Action** to each target repo (label-triggered
      check that reports lint results back to `/webhooks/github`). Template in
      [`.agents/standards/ci-integration.md`](.agents/standards/ci-integration.md). *Without this, the lint-fixer
      opens a PR but the loop never resumes.*
- [ ] **Set `GITHUB_TOKEN`** (repo scope) in `.env` so the lint-fixer can push/PR and
      private repos (e.g. `omnivore`) can be read.
- [ ] **Configure a notifier** (`SLACK_WEBHOOK_URL` or `TEAMS_WEBHOOK_URL`) so the
      summary digest and lint-fix results actually post.
- [ ] **Real end-to-end lint-fix run** against a live repo (needs the three items above)
      to validate kickoff → PR → CI → resume → success/needs-review.
- [ ] **Cloud deploy**: Cloud Run + Firestore (`SESSION_BACKEND=firestore`), Secret Manager,
      `LLM_PROVIDER=gemini` in prod (or Ollama on a GPU VM). With durable sessions a restart
      resumes cleanly, so Cloud Run can scale toward zero (Cloud Scheduler drives the cron +
      sweep). Full step-by-step + remaining infra TODOs in [`DEPLOYMENT.md`](DEPLOYMENT.md).
- [ ] **Port parity (Phase F)**: mirror the durable-session design (sessions + park store +
      `/internal` ingress + status-aware summaries) into Python / TS / Kotlin.

Nice-to-haves:

- [ ] Summary repo **org auto-discovery** (`GITHUB_ORG`) instead of a static `REPOS` list.
- [ ] **Eval** for lint-fix quality (the wiring is proven; fix quality depends on
      model/prompt — bigger Gemma models or Gemini improve it).
- [ ] **Orphan-session GC** + IaC/Terraform + OIDC for `/internal/*` (see [`DEPLOYMENT.md`](DEPLOYMENT.md) TODOs).

## Layout

| Path | Purpose |
|---|---|
| `cmd/agent` | service entrypoint |
| `cmd/playground` | local ADK web UI (dev only; never deployed) |
| `internal/agent` | root / summary / lintfixer / covfixer agents + shared `setup` + `fixflow` |
| `internal/{githubapi,gitrepo,webhook,notify,scheduler}` | deterministic tooling |
| `internal/{config,ingest}` | configuration + normalized event envelope |
| `ARCH/` | architecture-conformance tests |
| `.agents/` | standards, skills, and spec templates |
| `specs/` | developer memory (gitignored) — created from `.agents/templates` |

Every directory carries an `AGENTS.md`; the ARCH suite enforces it.
