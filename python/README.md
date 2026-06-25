# automation-agent (Python / ADK)

An automation service built on **Python + Google ADK**. The authoritative design is
[`../.agents/standards/architecture-design.md`](../.agents/standards/architecture-design.md).

> Implemented: config, ingest, notify, githubapi, gitrepo, webhook, the
> model layer, root + summary, and the fixflow engine behind the lint-fixer and
> coverage-fixer, all wired in `cmd/agent`.

## Quick start

```bash
cp .env.example .env      # then edit
make help                 # list all targets
make ci                   # lint + typecheck + arch + test + coverage gate
make run                  # run the service
make playground           # local ADK web UI at http://localhost:8080 (dev only)
```
