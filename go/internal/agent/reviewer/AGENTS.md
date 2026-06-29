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

## Intake pipeline

`Engine.Kickoff` runs a deterministic, model-free intake before any review work and produces
a `decision` (skip / deny / review):

1. **Parse** the event (`githubapi.ParsePullRequestEvent`).
2. **Trigger gate** — only `opened` / `reopened` / `synchronize` / `ready_for_review` proceed.
3. **Skip rules** (Decision 19) — draft (unless `ready_for_review`, `REVIEW_SKIP_DRAFTS`),
   the agent's own `automation-agent/*` branches, the `skip-review` label, and dependency-bot
   authors (`dependabot[bot]` / `renovate[bot]`).
4. **Fetch** the changed files + patches (`githubapi.ListPRFiles`, REST, paginated).
5. **Filter** generated/vendored/lockfile/minified/binary paths (`REVIEW_EXCLUDE_GLOBS`);
   size is computed on the **filtered** set. An empty filtered diff skips.
6. **Size gate** — two-dimensional (`REVIEW_MAX_FILES` **and** `REVIEW_MAX_DIFF_BYTES`):
   over either cap denies (review-or-deny, no degrade tier — Decision 4).

The category fan-out, scorecard, publishing the review, and posting the deny comment land in
later changes.

## Files

- `reviewer.go` — `Deps`, `Engine`, `NewEngine`, `Engine.Kickoff(ctx, raw)`, and the `decide`
  intake orchestration + skip helpers. Gated by `REVIEW_ENABLED` (default false, the kill
  switch). Like the lint/coverage fixers it keeps its constructor and logic in one file; the
  pure-wiring `agents_setup.go` (the build-agent split) arrives when the ADK category
  sub-agents do.
- `filter.go` — the exclude-glob `fileFilter` (basename and `**`-aware path globs) that drops
  generated/vendored/binary churn and totals the filtered patch bytes.
- `sizegate.go` — `oversize`, the two-dimensional file-count/diff-byte cap.

The `pull_request` webhook parse (`ParsePullRequestEvent`) and the file fetch (`ListPRFiles`)
live in `githubapi` next to `ParseCheckRunEvent`, so the SDK stays in the tooling layer and
the reviewer consumes stable projections — no `go-github` import here.

Wiring: `root` registers `KindReview` → `Engine.Kickoff`; `cmd` builds the engine (via
`NewEngine`) from config and injects the `githubapi` client. Provider SDKs stay out via
`setup` helpers. Tests are deterministic glue only — no assertions on LLM output.
