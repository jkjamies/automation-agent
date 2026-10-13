# ticketflow

An orchestration layer that takes a Jira ticket to a GitHub pull request with **no human
in the loop up to the PR, and no merge without a human**. Built in Go on Google's Agent
Development Kit (ADK) with models on Vertex AI.

The design, and the reasoning behind each choice, is in [docs/design.md](docs/design.md).

```
Jira ── label "agent-ready" ──▶ ticketflow ──▶ draft PR ──▶ CI ──▶ human review ──▶ human merge
                                    │
             intake → readiness gate → plan → implement ⇄ verify → self-review → publish
                         │              │                    │
                         └─ bounce ◀────┘ (questions)        └─ escalate (needs a human)
```

## How a run works

1. **Trigger.** Someone allowed to do so adds the `agent-ready` label. Jira calls
   `POST /webhooks/jira`; ticketflow authenticates the delivery and queues the ticket.
2. **Intake.** It re-reads the ticket from the Jira API (the webhook body is never trusted
   for content), resolves the repository, and applies the deterministic **readiness gate**:
   issue type, blocking labels, a real description, explicit acceptance criteria.
   It claims the ticket (`agent-in-progress`).
3. **Prepare.** It clones the repository and reads `.ticketflow.json` from the base commit:
   the checks a change must pass, extra protected paths, and agent instructions. A
   repository without this file is not touched.
4. **Plan.** A planner agent with read-only tools explores the repo and either writes a
   plan or declares the ticket not ready with specific questions.
5. **Implement ⇄ verify.** A **harness** edits the checkout. ticketflow then judges the
   result itself: it computes the diff from git, applies the **diff gate** (size limits,
   protected paths such as CI config, deleted tests, secret patterns), and runs every
   repository check. Failures go back to the harness as feedback, up to a budget.
6. **Self-review.** A reviewer agent reads the diff against the acceptance criteria.
   Blocking findings go back for one more round; anything still open is listed at the top
   of the PR.
7. **Publish.** Commit, push `ticketflow/<KEY>-<slug>`, open a draft PR, label it, comment
   on the ticket with the link, and move it to "In Review".

Every run ends at exactly one of: **publish** (PR open), **bounce** (questions posted on
the ticket, label removed), or **escalate** (`agent-needs-human`, reason posted).

## Harnesses

| `TICKETFLOW_HARNESS` | What edits the code |
|---|---|
| `adk` (default) | An in-process ADK `LlmAgent` on a Vertex AI model with confined file tools and the repo's named checks. Loads the repo's Agent Skills (`.agents/skills`, `.agent/skills`, `.gemini/skills`). |
| `cli` | Any headless coding agent started as a process in the checkout — e.g. Gemini CLI, or Antigravity if it gains a headless mode. Gets a scrubbed environment: no repo or Jira credentials. |

Either way, the harness's own claims are informational; the diff, the gates, and the
checks decide.

## Onboarding a repository

Add `.ticketflow.json` at the root:

```json
{
  "checks": [
    {"name": "build", "run": ["go", "build", "./..."]},
    {"name": "test",  "run": ["go", "test", "./..."]}
  ],
  "forbidden": ["db/migrations/**", "infra/**"],
  "instructions": "Use the repository's errors package; never log request bodies.",
  "max_files": 15
}
```

Then protect the default branch as described in
[docs/design.md § The merge gate](docs/design.md#the-merge-gate).

## Running it

```sh
cp .env.example .env   # fill in Jira, GitHub, and GOOGLE_CLOUD_PROJECT
set -a; . ./.env; set +a
gcloud auth application-default login

make test                                   # offline: fake model, local git remotes
go run ./cmd/ticketflow run -dry-run PROJ-123   # full run, writes nothing anywhere
go run ./cmd/ticketflow run PROJ-123            # opens a draft PR
go run ./cmd/ticketflow serve                   # webhook receiver on :8080
```

`-dry-run` does the planning, coding, checks, and self-review, then prints the PR
description and patch instead of pushing.

## Layout

| Package | Role |
|---|---|
| `cmd/ticketflow` | Entrypoint: `run` and `serve`; builds the Vertex AI models. |
| `internal/pipeline` | The ADK graph workflow and its nodes. |
| `internal/harness` | The `Harness` seam; ADK and CLI implementations. |
| `internal/gate` | Readiness gate, diff gate, repository policy. |
| `internal/repotools` | Path-confined ADK tools over a checkout. |
| `internal/workspace` | git (credentials never on disk, hooks disabled) and bounded commands. |
| `internal/jira`, `internal/github` | Minimal REST clients. The GitHub one has no merge call. |
| `internal/server` | Webhook authentication, trigger filter, de-duplication, worker pool. |
| `internal/prompts` | Every prompt and message template, as markdown. |
| `internal/config` | The only reader of the environment. |
