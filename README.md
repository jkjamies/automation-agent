# automation-agent

A lightweight, long-running Go service that ingests events from many sources
(Cloud Scheduler today; GitHub/Jira/Confluence/human later), routes every ingest through a
**root agent**, and runs three workflow agents:

- **Summary** — daily digest of the last 24h of commits across N repos,
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
  (`com.google.adk:google-adk-kotlin-core:0.4.0`). A functional 1:1 port (`./gradlew build`
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

How-to guides:
[`local-development.md`](.agents/standards/local-development.md) (run modes, env vars,
container), [`testing.md`](.agents/standards/testing.md) (every test kind + the Firestore
emulator), [`deployment.md`](.agents/standards/deployment.md) (cloud architecture + GCP
setup — source of truth), and [`ci-integration.md`](.agents/standards/ci-integration.md)
(how CI drives the fixers). [`DEPLOYMENT.md`](DEPLOYMENT.md) is the short status/checklist.

### Durable sessions

The fix loop's suspend/resume state is stored behind one `SESSION_BACKEND` switch —
`memory` (default, zero-dependency), `sqlite` (durable local file), or `firestore` (cloud,
scale-to-zero). Cloud Scheduler drives the daily digest and the timeout sweep via
`POST /internal/cron/daily` and `POST /internal/sweep` (Bearer-auth'd with
`INTERNAL_TOKEN`). With a durable backend a process restart resumes parked runs cleanly,
which is what lets Cloud Run scale toward zero.

## What's here

The full service is implemented in Go and `make ci` is green (≥80% coverage; Firestore
validated against the emulator): the summary, lint-fixer, and coverage-fixer workflows, the
root dispatcher, the deterministic tooling, and the durable-sessions design (the
`SESSION_BACKEND` switch, the `ParkStore` seam, Firestore session/park backends,
status-aware summaries, and the Cloud Scheduler `/internal` ingress). The core service runs
locally and the LLM steps are verified against real Gemma. The Kotlin, Python, and
TypeScript ports mirror it (each port's `ci` gate green).

To run against live repos and cloud infrastructure you supply the surrounding pieces:

- An **`agent-lint-verify` GitHub Action** in each target repo (a label-triggered check that
  reports lint results back to `/webhooks/github`; template in
  [`.agents/standards/ci-integration.md`](.agents/standards/ci-integration.md)). The
  lint-fixer opens a PR but the loop only resumes once this check reports.
- `GITHUB_TOKEN` (repo scope) so the lint-fixer can push/PR and read private repos.
- A notifier (`SLACK_WEBHOOK_URL` or `TEAMS_WEBHOOK_URL`) so the digest and fix results post.
- For cloud: Cloud Run + Firestore (`SESSION_BACKEND=firestore`), Secret Manager, and
  `LLM_PROVIDER=gemini` (or Ollama on a GPU VM). Durable sessions let a restart resume
  cleanly, so Cloud Run can scale toward zero with Cloud Scheduler driving the daily digest
  and sweep. Full step-by-step in [`DEPLOYMENT.md`](DEPLOYMENT.md).

Not yet implemented: summary repo org auto-discovery (`GITHUB_ORG`) in place of a static
`REPOS` list, an eval harness for lint-fix quality, orphan-session GC, IaC/Terraform, and
OIDC for `/internal/*` (see [`DEPLOYMENT.md`](DEPLOYMENT.md)).

## Layout

| Path | Purpose |
|---|---|
| `go/` | the canonical Go implementation (`cmd/`, `internal/`, `ARCH/`, `Makefile`) |
| `kotlin/` · `python/` · `javascript/` | the sibling ports, each mirroring `go/` |
| `.agents/` | standards, skills, and spec templates |
| `specs/` | developer memory (gitignored) — created from `.agents/templates` |

Inside `go/` (mirrored by each port):

| Path | Purpose |
|---|---|
| `go/cmd/agent` | service entrypoint |
| `go/cmd/playground` | local ADK web UI (dev only; never deployed) |
| `go/internal/agent` | root / summary / lintfixer / covfixer agents + shared `setup` + `fixflow` |
| `go/internal/{githubapi,gitrepo,webhook,notify}` | deterministic tooling |
| `go/internal/{config,ingest}` | configuration + normalized event envelope |
| `go/ARCH/` | architecture-conformance tests |

Every directory carries an `AGENTS.md`; the ARCH suite enforces it.
