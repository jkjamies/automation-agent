# automation-agent

A lightweight, long-running Go service that ingests events from many sources
(cron today; GitHub/Jira/Confluence/human later), routes every ingest through a
**root agent**, and runs two workflow agents:

- **Summary** — daily/weekly digest of the last 24h of commits across N repos,
  posted to Slack or Teams.
- **Lint-fixer** — consumes an agnostic lint payload, opens a PR with a fix, and
  loops (max 3) on CI feedback before posting a result. Suspend/resume is durable
  via GitHub itself (no local database).

Built on the [Agent Development Kit for Go](https://github.com/google/adk-go),
local-first on **Ollama + Gemma**, with a config switch to **Gemini/Vertex** for
cloud deployment.

> **Design doc:** [`docs/architecture.md`](docs/architecture.md) is the source of
> truth for the architecture and decisions.

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
      [`docs/deployment.md`](docs/deployment.md). Stateless — no DB (GitHub is the state).

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
| `internal/agent` | root / summary / lintfixer agents + shared `setup` |
| `internal/{githubapi,gitrepo,webhook,notify,scheduler,reconcile}` | deterministic tooling |
| `internal/{config,ingest}` | configuration + normalized event envelope |
| `ARCH/` | architecture-conformance tests |
| `.agents/` | standards, skills, and spec templates |
| `specs/` | developer memory (gitignored) — created from `.agents/templates` |

Every directory carries an `AGENTS.md`; the ARCH suite enforces it.
