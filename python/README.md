# automation-agent (Python / ADK)

The **Python + Google ADK** twin of the Go `automation-agent` service at the repo
root. It is kept **one-to-one** with the Go variant in behaviour, configuration,
endpoints, env vars, and workflows. The shared authoritative design is
[`../docs/architecture.md`](../docs/architecture.md); the parity contract is
[`../.agents/standards/language-parity.md`](../.agents/standards/language-parity.md).

> The full port is complete (see [`PORTING.md`](PORTING.md)): config, ingest, notify,
> githubapi, gitrepo, scheduler, webhook, the model layer, root + summary, and the
> fixflow engine behind the lint-fixer and coverage-fixer, all wired in `cmd/agent`.

## Quick start

```bash
cp .env.example .env      # then edit
make help                 # list all targets
make ci                   # lint + typecheck + arch + test + coverage gate
make run                  # run the service
make playground           # local ADK web UI at http://localhost:8080 (dev only)
```
