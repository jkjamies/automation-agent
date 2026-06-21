# automation-agent

A lightweight, long-running Go service that ingests events from many sources
(cron today; GitHub/Jira/Confluence/human later), routes every ingest through a
**root agent**, and runs three workflow agents:

- **Summary** — daily/weekly digest of the last 24h of commits across N repos,
  posted to Slack or Teams.
- **Lint-fixer** — consumes an agnostic lint payload, opens a PR with a fix, and
  loops (max 3) on CI feedback before posting a result. Suspend/resume rides on
  ADK long-running tools plus an in-memory parked-run registry (no database); a
  process restart strands in-flight runs — an accepted trade-off.
- **Coverage-fixer** — consumes an agnostic coverage report (JaCoCo, lcov, `go cover`,
  …) and opens a PR adding tests for *meaningful* uncovered logic, with the same CI
  loop. Shares the `fixflow` engine with the lint-fixer.

Built on the [Agent Development Kit for Go](https://github.com/google/adk-go),
local-first on **Ollama + Gemma**, with a config switch to **Gemini/Vertex** for
cloud deployment.

> **Design doc:** [`docs/architecture.md`](docs/architecture.md) is the source of
> truth for the architecture and decisions.

## Ports (Go · Kotlin · Python)

The Go implementation in [`go/`](go/) is the canonical reference. It is mirrored by
sibling ports that must stay **1:1 in functionality** — same structure, public surface,
config, and external contracts (see [`.agents/standards/language-parity.md`](.agents/standards/language-parity.md)):

- **Kotlin** — [`kotlin/`](kotlin/), built on [ADK for Kotlin](https://github.com/google/adk-kotlin)
  (`com.google.adk:google-adk-kotlin-core:0.2.0`). Port in progress; see
  [`kotlin/PORTING.md`](kotlin/PORTING.md).
- **Python** — [`python/`](python/), built on `google-adk` from PyPI. A functional 1:1
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

See also [`docs/ci-integration.md`](docs/ci-integration.md) (how CI sends lint
problems) and [`docs/deployment.md`](docs/deployment.md) (local + cloud).

## Status & TODO

Phases 1–5 are implemented and `make ci` is green (≥80% coverage per package). The
core service runs locally; the LLM steps are verified against real Gemma. Remaining
work to reach a fully production-validated system:

- [ ] **Add the `agent-lint-verify` GitHub Action** to each target repo (label-triggered
      check that reports lint results back to `/webhooks/github`). Template in
      [`docs/ci-integration.md`](docs/ci-integration.md). *Without this, the lint-fixer
      opens a PR but the loop never resumes.*
- [ ] **Set `GITHUB_TOKEN`** (repo scope) in `.env` so the lint-fixer can push/PR and
      private repos (e.g. `omnivore`) can be read.
- [ ] **Configure a notifier** (`SLACK_WEBHOOK_URL` or `TEAMS_WEBHOOK_URL`) so the
      summary digest and lint-fix results actually post.
- [ ] **Real end-to-end lint-fix run** against a live repo (needs the three items above)
      to validate kickoff → PR → CI → resume → success/needs-review.
- [ ] **Phase 6 — cloud deploy**: Cloud Run (`min-instances=1`) or GCE, Secret Manager,
      `LLM_PROVIDER=gemini` in prod (or Ollama on a GPU VM). Outline in
      [`docs/deployment.md`](docs/deployment.md). No DB; in-flight fix runs are tracked
      in-memory, so run a single instance (a restart strands parked runs).

Nice-to-haves:

- [ ] Summary repo **org auto-discovery** (`GITHUB_ORG`) instead of a static `REPOS` list.
- [ ] **Eval** for lint-fix quality (the wiring is proven; fix quality depends on
      model/prompt — bigger Gemma models or Gemini improve it).
- [ ] A shared lock/DB **only if** scaling to multiple instances (see architecture §8).

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
