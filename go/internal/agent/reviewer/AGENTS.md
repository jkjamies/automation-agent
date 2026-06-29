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

When the decision is **review**, the model-calling stage runs (see below). Publishing the
scorecard/inline comments and posting the deny comment land in later changes.

## Review stage (category fan-out → glue → scorecard)

When intake returns `review`, `Engine.review` runs the model-calling stage:

1. **Fan out** one agent per applicable category over the **whole filtered diff** (Decision 3),
   in parallel (ADK `ParallelAgent` — concurrent on Vertex, GPU-serialized locally). The
   consolidated set: Safety + Security + Code quality (code tier), Performance +
   Accessibility (base tier; accessibility only when UI/markup files changed) + an `(other)`
   catch-all whose findings are demoted to nitpick.
2. **Glue/synthesis** (code tier, always) runs over the diff + the category findings and adds
   the cross-cutting lenses: architectural alignment, testability, test coverage (Decision 3/12).
3. **Verify gate + dedup** (Decision 13/5, deterministic, in code — not asked of the model):
   drop findings below `REVIEW_MIN_CONFIDENCE`, then collapse cross-lens duplicates by
   fingerprint (keep worst severity).
4. **Scorecard** (Decision 5): a per-dimension severity histogram → level (🔴 any critical or
   ≥2 major · 🟡 any major or ≥3 medium · 🟢 else); overall = critical-cap (any critical in
   security / runtime safety → 🔴) combined with the worst dimension level. Count-based — no
   synthetic 0–100 score. For now the scorecard is logged; publishing lands later.

### Structured output on the local model path

adk-go v1.4.0's `OutputSchema` does **not** enforce a shape (validation is an unimplemented
TODO), and the Ollama adapter only forwards generic JSON mode via `ResponseMIMEType`. So
category agents request `application/json` (valid JSON syntax), describe the exact findings
schema in their prompt, and `parseFindings` recovers **defensively** — it extracts the first
JSON array from the model text, tolerates fences/prose, and treats a malformed body as no
findings (empty = success, Decisions 2/13). This is best-effort by design; the narrow
single-lens prompts are themselves the false-positive control, and the model is a config knob.

## Files

- `reviewer.go` — `Deps`, `Engine`, `NewEngine`, `Engine.Kickoff(ctx, raw)`, and the `decide`
  intake orchestration + skip helpers. Gated by `REVIEW_ENABLED` (default false, the kill
  switch).
- `filter.go` — the exclude-glob `fileFilter` (basename and `**`-aware path globs) that drops
  generated/vendored/binary churn and totals the filtered patch bytes.
- `sizegate.go` — `oversize`, the two-dimensional file-count/diff-byte cap.
- `findings.go` — the `Finding` schema, severity/dimension normalization, `fingerprint`, and
  the defensive `parseFindings`.
- `categories.go` — the consolidated category set + `selectCategories` (UI-only gating).
- `scorecard.go` — the count-based `scoreFindings`.
- `glue.go` — the deterministic verify gate + cross-lens `dedupe` the glue pass owns.
- `review.go` — `Engine.review`: the fan-out drive (`ParallelReview`), glue drive, and diff
  formatting.
- `agents_setup.go` — the build-agent split: pure ADK wiring (category + glue LLM agents, the
  prompt embed, the JSON `GenerateContentConfig`). Logic lives in the files above.
- `prompts/*.md` — one markdown prompt per category and the glue pass.

The `pull_request` webhook parse (`ParsePullRequestEvent`) and the file fetch (`ListPRFiles`)
live in `githubapi` next to `ParseCheckRunEvent`, so the SDK stays in the tooling layer and
the reviewer consumes stable projections — no `go-github` import here.

Wiring: `root` registers `KindReview` → `Engine.Kickoff`; `cmd` builds the engine (via
`NewEngine`) from config and injects the `githubapi` client. Provider SDKs stay out via
`setup` helpers. Tests are deterministic glue only — no assertions on LLM output.
