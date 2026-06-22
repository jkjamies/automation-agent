# Deployment

> **Status: Phase 6 draft.** The focus right now is **local runs**; the cloud
> sections are an outline to build out later.

## Local runs (current focus)

Prerequisites: Go 1.26, [Ollama](https://ollama.com) running with a Gemma model
(`ollama pull gemma3` / the project uses `gemma4:*`), and a `.env` (copy from
`.env.example`). All five run modes load `.env` automatically.

```bash
make run          # the full service: cron + webhooks (cmd/agent)
make playground   # local ADK web UI at http://localhost:8080 (cmd/playground)
make ci           # tidy + vet + arch + test + coverage gate
```

- **Summary** needs `REPOS` + a notifier (`SLACK_WEBHOOK_URL` or `TEAMS_WEBHOOK_URL`);
  otherwise it logs "disabled" and runs webhooks-only.
- **Lint-fixer** needs a `GITHUB_TOKEN` with repo scope to push/PR, and each target
  repo needs the `agent-lint-verify` workflow + a `check_run` webhook (see
  [`ci-integration.md`](ci-integration.md)).
- Exercise webhooks locally with `curl` against `http://localhost:8080/webhooks/...`.

### Local container

```bash
docker build -t automation-agent .
docker run --rm -p 8080:8080 --env-file .env \
  -e OLLAMA_HOST=http://host.docker.internal:11434 \
  automation-agent
```

The image builds **only** `cmd/agent` (the playground is never deployed). Point
`OLLAMA_HOST` at the host's Ollama, or set `LLM_PROVIDER=gemini` to use Vertex.

## Cloud (Phase 6 — outline, not yet built)

Recall the design constraints (`.agents/standards/architecture-design.md` §8, §13): the fix loop's state
is an **in-memory parked-run registry** (non-durable), so no persistent disk or
database is needed — but the trade-off is that a restart strands in-flight runs.
GitHub holds the durable PR artifacts but isn't consulted for recovery.

| Concern | Plan |
|---|---|
| Compute | Cloud Run with `min-instances=1` (keeps cron + webhook listener warm), or a GCE VM if co-locating Ollama on a GPU |
| Model in prod | `LLM_PROVIDER=gemini` (Vertex) unless a GPU VM runs Ollama |
| Secrets | Secret Manager for `GITHUB_TOKEN`, webhook secret, notifier URLs — not `.env` |
| State | in-memory parked-run registry (non-durable; a restart strands in-flight runs). GitHub holds the durable PR artifacts but isn't consulted for recovery |
| CI/CD | GitHub Actions building/pushing the image and deploying |
| Scale-out (later) | a shared lock/DB only if running multiple instances (see §8) |

When we build this phase out: add `agents-cli`-style infra or Terraform, a deploy
workflow, and wire Secret Manager. Until then, run locally.
