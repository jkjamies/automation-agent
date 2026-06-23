# Deployment

> **Status:** local runs are covered here; the **cloud** path now runs on durable sessions
> (Cloud Run + Firestore) and is fully written up in the repo-root
> [`DEPLOYMENT.md`](../../DEPLOYMENT.md) (Go reference). This file is the standards-level
> summary and cross-references it for ops detail — it does not duplicate the step-by-step.
> The Python / TS / Kotlin ports still mirror the older in-memory design (parity pending).

## Local runs (current focus)

Prerequisites: Go 1.26, [Ollama](https://ollama.com) running with a Gemma model
(`ollama pull gemma3` / the project uses `gemma4:*`), and a `.env` (copy from
`.env.example`). All five run modes load `.env` automatically.

```bash
make run                          # the full service: cron + webhooks (cmd/agent), SESSION_BACKEND=memory
SESSION_BACKEND=sqlite make run   # durable local: parked runs survive a restart (a local .db file)
make playground                   # local ADK web UI at http://localhost:8080 (cmd/playground)
make ci                           # tidy + vet + arch + test + coverage gate (memory + sqlite)
```

`SESSION_BACKEND` selects where the durable suspend/resume session **and** the park record
live: `memory` (default, in-process — a restart drops parked runs) | `sqlite` (durable local
file) | `firestore` (cloud). Testing the firestore code locally needs the emulator — see
[`DEPLOYMENT.md`](../../DEPLOYMENT.md).

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

## Cloud (Go reference — durable sessions)

The fix loop's suspend/resume state is held by a `SESSION_BACKEND`-selected `session.Service`
+ `setup.ParkStore` (see `.agents/standards/architecture-design.md` §8, §13). With a durable
backend (`firestore` in prod) a restart **resumes** in-flight runs cleanly rather than
stranding them — which is what lets Cloud Run scale toward zero. GitHub still holds the durable
PR artifacts; the agent doesn't scan them for recovery.

```
 GitHub repo ──webhook(HMAC)──► POST /webhooks/{lint,coverage,github}
 Cloud Scheduler ─bearer─►       POST /internal/cron/{daily,weekly}   (digests)
 Cloud Scheduler ─bearer─►       POST /internal/sweep                 (timeout catch-all)
                                         │
                                    Cloud Run service
                                         │
                       ┌─────────────────┴─────────────────┐
                  session.Service                       ParkStore
                  (suspend/resume history)         (prKey→session, attempts, params)
                  memory | sqlite | firestore     memory | sqlite | firestore
```

| Concern | Plan |
|---|---|
| Compute | **Cloud Run + `SESSION_BACKEND=firestore`** (scale-to-zero, durable). Or `min-instances=1` / a GCE VM with `SESSION_BACKEND=memory` for the lightweight mode (a restart then strands runs). |
| Model in prod | `LLM_PROVIDER=gemini` (Vertex) unless a GPU VM runs Ollama |
| Secrets | Secret Manager for `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET`, `INTERNAL_TOKEN`, notifier URLs — not `.env` |
| State | `session.Service` + `ParkStore`, both `SESSION_BACKEND`-switched (`firestore` = durable, scale-to-zero). Eager terminal cleanup deletes both record and session so a durable backend doesn't leak. |
| Scheduler | Cloud Scheduler → `/internal/cron/{daily,weekly}` + `/internal/sweep` (Bearer `INTERNAL_TOKEN`). **Caution:** don't also let the in-process cron fire the digests (see `DEPLOYMENT.md`). |
| CI/CD | GitHub Actions building/pushing the image and deploying |
| HA / scale-out | `firestore` is a shared store with atomic single-winner claims, so replicas can in principle share it; not exercised yet |

The full ops walkthrough — Firestore setup, ADC roles, the three Cloud Scheduler jobs, the
firestore emulator for local tests, and the pending TODOs (Terraform/IaC, orphan-session GC,
the in-process-scheduler disable flag, parity to the ports) — lives in
[`DEPLOYMENT.md`](../../DEPLOYMENT.md). It is not duplicated here.
