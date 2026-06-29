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

## Coalesce-to-latest — debounce + staleness

Rapid pushes to one PR are collapsed so only the latest SHA is reviewed (two parts, because
Cloud Tasks gives no ordering and cannot cancel an in-flight task):

- **Debounce at enqueue** (`enqueue.go`, `EnqueueOptions`): a `synchronize` review is enqueued
  with `REVIEW_DEBOUNCE` delay under a per-PR Cloud Tasks dedup name, so a burst of pushes
  collapses to one delayed task. `opened`/`reopened`/`ready_for_review` enqueue immediately. This
  is a workflow concern, so it lives here, not in the transport. Only the Cloud Tasks backend
  honors the hints.
- **Staleness at execution** (`Kickoff` → `superseded`): before doing the review work, the engine
  fetches the PR's current head SHA and skips if it no longer matches the event's SHA (a newer
  push won). Best-effort — a lookup error proceeds rather than suppressing a real review.

**Incremental re-review** (re-evaluating only the files changed since the last reviewed SHA) is
intentionally **not** built: GitHub-as-store persists rendered comments, not structured findings,
so the latest SHA is always reviewed in full and reconciled against the existing comments.

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

### Reconciliation — GitHub-as-store (no local durable state)

Re-reviewing a PR is **idempotent and self-cleaning** without a local store: every inline comment
carries a hidden fingerprint marker `<!-- ar-fp:<file:line:normalizedMessage> -->`, so GitHub itself
holds the per-PR review state. On each publish (`reconcile.go` + `publish.go`):

- **list** the PR's existing review comments (`ListReviewComments`, REST) and parse their markers;
- **keep** a finding already represented by a comment (not re-posted → idempotent);
- **add** a finding with no existing comment (posted with its marker);
- **minimize** an existing fingerprinted comment whose finding is gone — collapsed as **OUTDATED**
  via `MinimizeComment` (the **only GraphQL** in the codebase: a raw mutation to `<BaseURL>/graphql`
  over the same `TokenProvider`, since the REST API has no minimize/resolve). Comments without our
  marker (foreign, or pre-reconciliation) are ignored.

Minimization is **best-effort**: it runs after the new inline comments are posted but a single
`MinimizeComment` failure only logs and continues so the summary comment and check run still
publish (a leftover stale comment is collapsed on the next genuine re-push). Reconciliation keys
purely off the hidden `ar-fp:` marker and does **not** filter by comment author: at a single
deployment every marked comment is one this agent posted, so closing-its-own-fixed-comments holds
without identity resolution. Author/thread-identity awareness is **deferred to the future
reply-to-reply feature** (which inherently needs to know which thread is the agent's and who
replied); the cheap marker-scoping fix covers the only residual case (two deployments sharing one
repo) if it ever becomes real.

This replaced the publish stage's coarse whole-SHA skip **for inline comments only**. The
`alreadyPublished` head-SHA guard still protects the non-comment outputs — the summary comment, the
`agent-review` check run, and the `publishDeny` path — from duplicating on a redelivered task (a
genuine re-push carries a new SHA and reconciles normally). Still to come (later changes):
**reply-to-reply** threading. (**Debounce** is built — see *Coalesce-to-latest* above; **incremental
re-review** is intentionally deferred — see the note under that section.) The read-aggregate path may
move to GraphQL if pilot volume justifies it; patches stay REST (GraphQL cannot return diff hunks;
`createCheckRun` is also REST-only).

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

When the decision is **review**, the model-calling stage runs (see below) and its result is
published; when it is **deny**, the "too large" summary + a neutral check are published.

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
   synthetic 0–100 score.

## Publish stage (CodeRabbit-style, advisory, all REST)

`Engine.publish` posts the scored review; nothing here gates a merge (Decision 15):

1. **Classify** the gated findings against the diff hunks (`hunks.go`): actionable
   (critical/major/medium) findings on a commentable head-side line post **inline**; actionable
   findings outside the diff are listed in the summary's **🔭 Outside diff range** section (never
   dropped or snapped to a wrong line — Decision 6); nitpicks collapse into **🧹 Nitpicks**.
2. **Inline comments** carry an icon+category prefix (`🔒 Security` / `⚠️ Potential issue` /
   `🛠️ Refactor`), an optional ```suggestion block, and an optional collapsible **🤖 Prompt for
   AI agents** block (`fix_prompt`, Decision 10), posted as one advisory `COMMENT` review.
3. **Reconcile** against the PR's existing fingerprinted comments (see *Reconciliation* above):
   skip findings already posted (idempotent), post only new ones, and minimize comments now fixed.
4. **Summary comment** is marker-updated (`<!-- automation-agent:review:<owner>/<repo>#<n> -->`,
   Decision 9) so a re-review edits it in place: header + scorecard table + the collapsible
   sections + review details (head SHA, file count, tiers).
5. **`agent-review` check** (advisory): green → `success`, yellow/red → `neutral` — **never**
   `failure`. Deny publishes the "too large, please split" summary + a neutral check.

Still to come: reply-to-reply threading and standards-aware review. (Debounce is built;
incremental re-review is intentionally deferred — see the *Coalesce-to-latest* section.)

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
  formatting. Returns the scorecard and the gated findings for the publish stage.
- `hunks.go` — `commentableLines` / `diffIndex.inDiff`: which head-side lines of a patch GitHub
  accepts an inline comment on (added/context lines), used to route in-diff vs out-of-diff.
- `publish.go` — `Engine.publish` / `Engine.publishDeny`: the CodeRabbit-style assembly + REST
  writes (advisory review, marker summary comment, advisory `agent-review` check).
- `reconcile.go` — the fingerprint marker (`fpMarker`/`parseFPMarker`) and the pure `reconcile`:
  given this run's inline findings + the PR's existing comments, what to post vs minimize.
- `agents_setup.go` — the build-agent split: pure ADK wiring (category + glue LLM agents, the
  prompt embed, the JSON `GenerateContentConfig`). Logic lives in the files above.
- `prompts/*.md` — one markdown prompt per category and the glue pass.

The `pull_request` webhook parse (`ParsePullRequestEvent`) and the file fetch (`ListPRFiles`)
live in `githubapi` next to `ParseCheckRunEvent`, so the SDK stays in the tooling layer and
the reviewer consumes stable projections — no `go-github` import here.

Wiring: `root` registers `KindReview` → `Engine.Kickoff`; `cmd` builds the engine (via
`NewEngine`) from config and injects the `githubapi` client. Provider SDKs stay out via
`setup` helpers. Tests are deterministic glue only — no assertions on LLM output.
