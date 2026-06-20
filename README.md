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
```

## Layout

| Path | Purpose |
|---|---|
| `cmd/agent` | service entrypoint |
| `internal/agent` | root / summary / lintfixer agents + shared `setup` |
| `internal/{githubapi,gitrepo,webhook,notify,scheduler,reconcile}` | deterministic tooling |
| `internal/{config,ingest}` | configuration + normalized event envelope |
| `ARCH/` | architecture-conformance tests |
| `.agents/` | standards, skills, and spec templates |
| `specs/` | developer memory (gitignored) — created from `.agents/templates` |

Every directory carries an `AGENTS.md`; the ARCH suite enforces it.
