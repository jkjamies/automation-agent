# internal/agent/reviewer

The in-house **PR code-review** workflow. It reacts to GitHub `pull_request` events and is
being built to post a CodeRabbit-style review — per-category sub-agent findings, a
count-based scorecard, inline comments with ```suggestion blocks, an "🤖 Prompt for AI
agents" block, and an **advisory** `agent-review` check (never a merge gate). It is
**comment-only** and never opens PRs. See `specs/20260625-pr-code-review-agent.md`.

Unlike the lint/coverage fixers, the reviewer is **not** a suspend/resume fix loop: it is
mostly one-shot per `pull_request` event and does not park on `await_ci`. Its long LLM
compute runs **in-request** via the execution transport (`KindReview` → `/internal/dispatch`),
so CPU stays allocated on Cloud Run.

## Trigger — native-event kickoff

The reviewer's kickoff is a **native GitHub event** (`pull_request`), not a custom POST
route. The GitHub App delivers it to the single `/webhooks/github` URL, where the handler
routes by the `X-GitHub-Event` header (`pull_request` → `KindReview`, `check_run` → `KindCI`).
This is a third door alongside the fixers' custom-route kickoff and native `check_run`
resume — see `.agents/standards/webhooks.md`.

## Data layer — REST-first (GraphQL deferred to reconciliation)

The reviewer reads the PR and posts its output via the **GitHub REST API**, over the shared
`auth.TokenProvider` (App installation token in production, PAT locally — no auth work here):

- **Read:** changed files **and patches** via `GET /pulls/{n}/files` (paginated); file
  content at the head SHA via `githubapi.GetFileContent`; PR metadata, labels, and check
  runs via REST.
- **Write:** the review (`POST /pulls/{n}/reviews` — inline comments + ```suggestion), the
  marker summary comment (issue comments), and the `agent-review` check run.
- **The `agent-review` check is REST-only** — GitHub's GraphQL API has no check-run
  mutation, so there is no GraphQL path for it by design.

### Future: GraphQL (only where REST cannot reach it)

GraphQL is **not** a prerequisite and is **not** used in the ingress/core/publish path.
It is added later, as a small module, only for the **reconciliation thread layer**:
resolving/minimizing a review thread whose finding is gone (fixed). `resolveReviewThread`
and `minimizeComment` are **GraphQL-only**; until that module lands, reconciliation degrades
to delete-stale (`DELETE /pulls/comments/{id}`). GraphQL rides the **same** `TokenProvider`,
so adding it is zero auth rework. If pilot volume ever justifies it, the read-aggregate path
(threads/comments/metadata — **not** patches, which GraphQL cannot return) may also move to
GraphQL. (Corrected 2026-06-29: the earlier "GraphQL-native data layer" plan rested on the
mistaken belief that GraphQL exposes file patches and a `createCheckRun` mutation — it does
neither.)

## Files

- `reviewer.go` — `Deps`, `Engine`, `NewEngine`, and `Engine.Kickoff(ctx, raw)`: the
  per-`pull_request` logic. Currently the ingress slice — gated by `REVIEW_ENABLED` (default
  false, the kill switch); the diff fetch, category fan-out, scorecard, and publish land in
  later changes. Like the lint/coverage fixers, this engine keeps its constructor and logic
  in one file; the pure-wiring `agents_setup.go` (the build-agent split) arrives when the ADK
  category sub-agents do.

Wiring: `root` registers `KindReview` → `Engine.Kickoff`; `cmd` builds the engine (via
`NewEngine`) from config. Tooling (`githubapi`) is injected; provider SDKs stay out via
`setup` helpers. Tests are deterministic glue only — no assertions on LLM output.
