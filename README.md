# automation-agent

A lightweight, long-running Go service that ingests events from many sources
(Cloud Scheduler today; GitHub/Jira/Confluence/human later), routes every ingest through a
**root agent**, and runs four workflow agents:

- **Summary** — daily digest of the last 24h of commits across N repos,
  posted to Slack or Teams.
- **Lint-fixer** — consumes an agnostic lint payload, opens a PR with a fix, and
  loops (max 3) on CI feedback before posting a result. Suspend/resume rides on a
  workflow graph whose `await_ci` node pauses on a request-input interrupt, plus a
  **pluggable durable backend** (`SESSION_BACKEND` =
  `memory` | `sqlite` | `firestore`): with a durable backend a process restart
  **resumes cleanly** instead of stranding in-flight runs, and terminal results post
  a status-aware summary (what changed on the PR + the targeted findings).
- **Coverage-fixer** — consumes an agnostic coverage report (JaCoCo, lcov, `go cover`,
  …) and opens a PR adding tests for *meaningful* uncovered logic, with the same CI
  loop. Shares the `fixflow` engine with the lint-fixer.
- **Reviewer** — an in-house PR code reviewer: one-shot, advisory, comment-only, and
  steered off the reviewed repo's own standards docs. Off by default: it needs both
  `REVIEW_ENABLED=true` **and** the GitHub App subscribed to the **Pull request** event
  (with either missing it is silent rather than erroring).

Built on the [Agent Development Kit for Go](https://github.com/google/adk-go),
local-first on **Ollama + Gemma**, with a config switch to **Gemini/Vertex** for
cloud deployment.

> **Design doc:** [`okf/standards/architecture-design.md`](okf/standards/architecture-design.md) is the source of
> truth for the architecture and decisions.

## Quick start

```bash
cp .env.example .env      # then edit
make help                 # list all targets
make ci                   # tidy-check + vet + lint + arch + test + coverage gate (read-only)
make run                  # run the service
make playground           # local ADK web UI at http://localhost:8080 (dev only)
```

How-to guides:
[`local-development.md`](okf/standards/local-development.md) (run modes, env vars,
container), [`testing.md`](okf/standards/testing.md) (every test kind + the Firestore
emulator), [`deployment.md`](okf/standards/deployment.md) (cloud architecture + GCP
setup — source of truth), and [`ci-integration.md`](okf/standards/ci-integration.md)
(how CI drives the fixers). [`DEPLOYMENT.md`](DEPLOYMENT.md) is the short status/checklist.

### Durable sessions

The fix loop's suspend/resume state is stored behind one `SESSION_BACKEND` switch —
`memory` (default, zero-dependency), `sqlite` (durable local file), or `firestore` (cloud,
scale-to-zero). Cloud Scheduler drives the daily digest and the timeout sweep via
`POST /internal/cron/daily` and `POST /internal/sweep` (Bearer-auth'd with
`INTERNAL_TOKEN`). With a durable backend a process restart resumes parked runs cleanly,
which is what lets Cloud Run scale toward zero.

## What's here

The service is written in Go: the summary, lint-fixer, coverage-fixer, and reviewer
workflows, the root dispatcher, the deterministic tooling, and durable sessions (the
`SESSION_BACKEND` switch, the `ParkStore` seam, Firestore session/park backends,
status-aware summaries, and the Cloud Scheduler `/internal` ingress, whose sweep both frees
runs whose CI never reported and reaps runs nothing can resolve after `ORPHAN_TTL`). It runs
locally against a real Ollama model; `make ci` is the gate, and
[testing](okf/standards/testing.md) describes what that covers.

To run against live repos and cloud infrastructure you supply the surrounding pieces:

- An **`agent-lint-verify` GitHub Action** in each target repo (a label-triggered check that
  reports lint results back to `/webhooks/github`; template in
  [`okf/standards/ci-integration.md`](okf/standards/ci-integration.md)). The
  lint-fixer opens a PR but the loop only resumes once this check reports.
- **GitHub credentials.** Production authenticates as a **GitHub App** (short-lived,
  repo-scoped installation tokens): `GITHUB_APP_ID` + `GITHUB_APP_INSTALLATION_ID` + the
  private-key PEM, with the App subscribed to **Check run** and **Pull request**. Omit those
  and the service falls back to a PAT (repo scope) for local dev, resolved in order:
  `GITHUB_TOKEN`, then `GH_TOKEN`, then whatever `gh auth token` reports from an existing
  `gh auth login`. That last step is skipped in App mode, so a deployment never silently
  picks up a developer's PAT. Either way this is the **GitHub REST API** credential and is
  always required, even over SSH. For
  local dev you can clone/push over SSH instead of an https token by setting
  `GIT_TRANSPORT=ssh` (uses your ssh-agent / `~/.ssh` keys, with `GIT_SSH_KEY` to pin a
  specific key) — but SSH only authenticates the git transport, so you **still** need a
  token or `gh auth login` for the PR operations. See
  [`local-development.md`](okf/standards/local-development.md).
- A notifier (`SLACK_WEBHOOK_URL` or `TEAMS_WEBHOOK_URL`) so the digest and fix results post.
- For cloud: Cloud Run + Firestore (`SESSION_BACKEND=firestore`), Secret Manager, and
  `LLM_PROVIDER=gemini` (or Ollama on a GPU VM). Durable sessions let a restart resume
  cleanly, so Cloud Run can scale toward zero with Cloud Scheduler driving the daily digest
  and sweep. Full step-by-step in [`DEPLOYMENT.md`](DEPLOYMENT.md).

Deliberate boundaries, so you know what you are not getting: summary repos come from a
static `REPOS` list rather than org auto-discovery (`GITHUB_ORG`); there is no eval harness
scoring lint-fix quality; deployment is the documented `gcloud` steps rather than
IaC/Terraform; and `/internal/*` is guarded by a bearer token rather than OIDC (see
[`DEPLOYMENT.md`](DEPLOYMENT.md)).

## Layout

| Path | Purpose |
|---|---|
| `go/` | the canonical Go implementation (`cmd/`, `internal/`, `ARCH/`, `Makefile`) |
| `.agents/` | skills and spec templates |
| `specs/` | developer memory (gitignored) — created from `.agents/templates` |

Inside `go/`:

| Path | Purpose |
|---|---|
| `go/cmd/agent` | service entrypoint |
| `go/cmd/playground` | local ADK web UI (dev only; never deployed) |
| `go/internal/agent` | root / summary / lintfixer / covfixer / reviewer agents + shared `setup` + `fixflow` |
| `go/internal/{githubapi,gitrepo,webhook,notify,tasks,obs}` | deterministic tooling (`tasks` = execution transport; `obs` = distributed tracing, off by default) |
| `go/internal/{config,ingest}` | configuration + normalized event envelope |
| `go/ARCH/` | architecture-conformance tests |

System knowledge lives in the [`okf/`](okf/index.md) bundle (Open Knowledge Format);
the ARCH suite enforces its conformance. The repo-root `AGENTS.md` is the guardrail
sheet + entry pointer.

## License

[MIT](LICENSE) — take it, change it, ship it; keep the copyright notice with any copy.
