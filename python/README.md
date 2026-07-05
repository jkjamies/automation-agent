# automation-agent (Python / ADK)

An automation service built on **Python + Google ADK**. The authoritative design is
[`../okf/standards/architecture-design.md`](../okf/standards/architecture-design.md); see
[`../okf/modules/ports/python.md`](../okf/modules/ports/python.md) for this port's concept.

## Quick start

```bash
cp .env.example .env      # then edit
make help                 # list all targets
make ci                   # lint + typecheck + arch + test + coverage gate
make run                  # run the service
make playground           # local ADK web UI at http://localhost:8080 (dev only)
```
