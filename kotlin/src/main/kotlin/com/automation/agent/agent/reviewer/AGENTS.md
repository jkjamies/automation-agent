# agent/reviewer

The in-house **PR code-review** workflow. It reacts to GitHub `pull_request` events and posts a
CodeRabbit-style review — per-category sub-agent findings, a count-based scorecard, inline comments
with ```suggestion blocks, an "🤖 Prompt for AI agents" block, and an **advisory** `agent-review`
check (never a merge gate). It is **comment-only** and never opens PRs.

Unlike the lint/coverage fixers, the reviewer is **not** a suspend/resume fix loop: it is mostly
one-shot per `pull_request` event and does not park on `await_ci`. Its long compute runs **in-request**
via the execution transport (`Kind.REVIEW` → `/internal/dispatch`), so CPU stays allocated on Cloud
Run.

## Flow

```mermaid
flowchart TD
    EV["pull_request event"] --> WH["/webhooks/github — route on X-GitHub-Event → Kind.REVIEW"]
    WH --> EQ{action}
    EQ -->|synchronize| DEB["enqueueOptions: debounce + per-PR-per-window coalesce name (REVIEW_DEBOUNCE)"]
    EQ -->|"opened / reopened / ready_for_review"| IMM["enqueue immediately"]
    DEB --> DISP["/internal/dispatch (in-request transport — CPU stays allocated)"]
    IMM --> DISP
    DISP --> KO["Engine.kickoff"]
    KO --> STALE{"superseded? current head SHA ≠ event SHA"}
    STALE -.->|newer push won| SKST["skip (stale)"]
    STALE --> DEC["decide — deterministic, model-free intake"]
    DEC --> D1["parse → trigger gate → skip rules (draft / own branch / skip-review / dep-bot)"]
    D1 --> D2["listPRFiles → exclude-glob filter → two-dimensional size gate"]
    D2 --> DK{decision}
    DK -->|skip| NOOP["no-op (log)"]
    DK -->|"deny (too large)"| DENY["publishDeny — 'please split' summary + neutral check"]
    DK -->|review| STD["discoverStandards — distill the repo's own convention docs (API-only, cached; generic on any error)"]
    STD --> FAN(["runReview — fan out (ADK ParallelAgent; concurrent on the cloud backend, GPU-serialized locally). Each lens sees the whole filtered diff + the standards menu"])
    FAN --> C1["Safety<br/>code tier"]
    FAN --> C2["Security<br/>code tier"]
    FAN --> C3["Code quality<br/>code tier"]
    FAN --> C4["Performance<br/>base tier"]
    FAN --> C5["Accessibility<br/>base tier · UI/markup only"]
    FAN --> C6["(other) catch-all → nitpick<br/>base tier"]
    C1 --> GLUE
    C2 --> GLUE
    C3 --> GLUE
    C4 --> GLUE
    C5 --> GLUE
    C6 --> GLUE
    GLUE["glue synthesis (code tier, always): architecture · testability · coverage"]
    GLUE --> GATE["verify gate (REVIEW_MIN_CONFIDENCE) + citation gate + cross-lens dedupe"]
    GATE --> SCORE["scorecard: per-dimension level + critical-cap → 🔴/🟡/🟢"]
    SCORE --> PUB["publish — reconcile inline comments, upsert the marker summary, create the advisory agent-review check"]
```

## Trigger — native-event kickoff

The reviewer's kickoff is a **native GitHub event** (`pull_request`), not a custom POST route. The
GitHub App delivers it to the single `/webhooks/github` URL, where the handler routes by the
`X-GitHub-Event` header (`pull_request` → `Kind.REVIEW`, `check_run` → `Kind.CI`, anything else → 200
no-dispatch).

## Coalesce-to-latest — debounce + staleness

Rapid pushes to one PR are collapsed so only the latest SHA is reviewed (two parts, because the task
queue gives no ordering and cannot cancel an in-flight task):

- **Debounce at enqueue** (`Enqueue.kt`, `enqueueOptions`): a `synchronize` review is enqueued with
  `REVIEW_DEBOUNCE` delay under a per-PR-per-window dedup name, so a burst of pushes collapses to one
  delayed task. The name carries a time bucket (receipt time floored to the debounce window) so a push
  minutes later doesn't collide with the queue's ~1h name reservation. `opened`/`reopened`/
  `ready_for_review` enqueue immediately. Only the Cloud Tasks backend honors the hints.
- **Staleness at execution** (`Engine.kickoff` → `superseded`): before doing the review work, the
  engine fetches the PR's current head SHA and skips if it no longer matches the event's SHA.
  Best-effort — a lookup error proceeds rather than suppressing a real review.

**Incremental re-review** is intentionally **not** built: GitHub-as-store persists rendered
comments, not structured findings, so the latest SHA is always reviewed in full and reconciled
against the existing comments.

## Data layer — REST-first (GraphQL only for minimize)

The reviewer reads the PR and posts its output via the GitHub REST API (`githubapi.Client`), over
the shared auth provider (App installation token in production, PAT locally):

- **Read:** changed files + patches (`listPRFiles`), file content at the head SHA (`getFileContent`),
  the repo tree (`tree`), the head SHA (`pullRequestHeadSha`), and the existing review comments
  (`listReviewComments`).
- **Write:** the review (`createReview` — inline comments + ```suggestion), the marker summary
  comment (`upsertMarkerComment`), and the `agent-review` check run (`createCheckRun`).
- **The `agent-review` check is REST-only** — GraphQL has no check-run mutation. The **only** GraphQL
  is `minimizeComment` (collapse an outdated comment as `OUTDATED`); its endpoint derives from the
  REST base incl. the GitHub Enterprise Server `/api/v3` → `/api/graphql` mapping.

### Reconciliation — GitHub-as-store (no local durable state)

Every inline comment carries a hidden fingerprint marker `<!-- ar-fp:<file:line:normalizedMessage>
-->`, so GitHub itself holds the per-PR review state. On each publish (`Reconcile.kt` + `Publish.kt`):
**keep** a finding already represented by a comment (idempotent), **add** a finding with no existing
comment, and **minimize** an existing fingerprinted comment whose finding is gone. Minimization is
best-effort — a single failure logs and continues so the summary and check still publish. The
`alreadyPublished` head-SHA guard protects the non-comment outputs (summary, check, deny) from
duplicating on a redelivered task. The marker-comment upsert edits only a comment this service could
have authored (`ownsComment`: the resolved authored login, else the App bot-type fallback).

## Intake pipeline

`Engine.decide` runs a deterministic, model-free intake and produces a `Decision` (skip / deny /
review):

1. **Parse** the event (`githubapi.parsePullRequestEvent`).
2. **Trigger gate** — only `opened` / `reopened` / `synchronize` / `ready_for_review` proceed.
3. **Skip rules** — draft (unless `ready_for_review`), the agent's own `automation-agent/*` branches,
   the `skip-review` label, and dependency-bot authors.
4. **Fetch** the changed files + patches (`listPRFiles`).
5. **Filter** generated/vendored/lockfile/minified/binary paths (`Filter.kt`); size is computed on the
   **filtered** set. An empty filtered diff skips.
6. **Size gate** — two-dimensional (`REVIEW_MAX_FILES` **and** `REVIEW_MAX_DIFF_BYTES`): over either
   cap denies (review-or-deny, no degrade tier).

## Review stage (category fan-out → glue → scorecard)

`Review.kt`: **fan out** one agent per applicable category over the whole filtered diff, in parallel
(ADK `ParallelAgent`). The consolidated set: Safety + Security + Code quality (code tier), Performance
+ Accessibility (base tier; accessibility only when UI/markup changed) + an `(other)` catch-all demoted
to nitpick. The **glue/synthesis** pass (code tier, always) adds architectural alignment, testability,
test coverage. Then the deterministic gates in code (`Glue.kt`, `Standards.kt`): drop below
`REVIEW_MIN_CONFIDENCE`, apply the standards citation gate, collapse cross-lens duplicates by
fingerprint. The **scorecard** (`Scorecard.kt`) is a per-dimension severity histogram → level (🔴 any
critical or ≥2 major · 🟡 any major or ≥3 medium · 🟢 else); overall = critical-cap (any critical in
security / runtime safety → 🔴) combined with the worst dimension.

ADK-Kotlin has no `LlmAgent.OutputKey`, so each lens is a code agent that calls its tier model directly
with the JSON generate-content config and emits its raw findings text — a category lens to its own
session-state key (read back by the parallel drive), the glue lens as the event content (read back by
the single-agent drive). Because the lens drives the model itself (not through an ADK tool runner),
the lazy `get_rule` standards drill-down is served by a small in-lens tool loop.

## Standards-aware review

`Standards.kt` steers off the conventions of the repo **under review** — `.agents/standards`,
`.cursor/rules`, `CLAUDE.md`, whatever that repo has. All API-only (no clone): **discover** the tree
and match `REVIEW_STANDARDS_GLOBS` (per-module scoping — a per-directory instruction file applies only
to touched modules), **distill** the docs with the base-tier model into one uniform tagged rule list,
**cache** per repo + docs SHA, **inject** the compact rule menu into every lens (with a lazy `get_rule`
tool for full text), and **gate citations** (`REVIEW_UNCITED_MODE`): a conformance finding that cites
no real injected `rule_id` is dropped or demoted to nitpick. A truncated tree, no docs, or a
distill/fetch error degrades to a **generic** review (uncached).

## Publish stage (CodeRabbit-style, advisory, all REST)

`Publish.kt` posts the scored review; nothing here gates a merge. Actionable (critical/major/medium)
findings on a commentable head-side line (`Hunks.kt`) post **inline**; actionable findings outside the
diff go to the summary's **🔭 Outside diff range** section (never snapped to a wrong line); nitpicks
collapse into **🧹 Nitpicks**. The summary is marker-updated
(`<!-- automation-agent:review:<owner>/<repo>#<n> -->`) so a re-review edits it in place. The
`agent-review` check: green → `success`, yellow/red → `neutral` — **never** `failure` (constrained at
the client boundary). Deny posts the "too large, please split" summary + a neutral check.

## Structured output

Category agents request `application/json` and describe the exact findings schema in their prompt;
`parseFindings` recovers **defensively** — it extracts the first decodable JSON array from the model
text, tolerates fences/prose, and treats a malformed body as no findings (empty = success). A
non-finite `confidence` (NaN/Infinity) is rejected at the array-validation layer. Best-effort by design;
the narrow single-lens prompts are themselves the false-positive control.

## Files

- `Reviewer.kt` — `Deps`, `Engine`, `newEngine`, `Engine.kickoff`, and the `decide` intake + skip
  helpers. Gated by `REVIEW_ENABLED` (default false, the kill switch).
- `Filter.kt` — the exclude-glob `FileFilter` and the filtered patch-byte total.
- `SizeGate.kt` — `oversize`, the two-dimensional file-count/diff-byte cap.
- `Findings.kt` — the `Finding` schema, severity/dimension normalization, `fingerprint`, and the
  defensive `parseFindings`.
- `Categories.kt` — the consolidated category set + `selectCategories` (UI-only gating).
- `Scorecard.kt` — the count-based `scoreFindings`.
- `Glue.kt` — the deterministic verify gate + cross-lens `dedupe`.
- `Review.kt` — `runReview`: the fan-out drive, glue drive, diff formatting, and instruction
  composition (incl. the standards menu).
- `Hunks.kt` — `commentableLines` / `DiffIndex.inDiff`: which head-side lines take an inline comment.
- `Publish.kt` — `publish` / `publishDeny`: the CodeRabbit-style assembly + REST writes.
- `Reconcile.kt` — the fingerprint marker and the pure `reconcile`.
- `Standards.kt` — discovery, the distiller orchestration + defensive `parseRules`, the per-repo
  `StandardsCache`, the rule menu / lazy `get_rule` tool, and `gateCitations`.
- `Enqueue.kt` — `enqueueOptions`: the debounce/coalesce transport hints.
- `AgentsSetup.kt` — pure ADK wiring (category + glue + distiller review agents, the prompt loader,
  the JSON generate-content config, and the in-lens `get_rule` tool loop).
- `prompts/reviewer/*.md` — one markdown prompt per category, the glue pass, and the standards
  distiller (in `resources`).

Wiring: `root` registers `Kind.REVIEW` → `Engine.kickoff`; `app` builds the engine (via `newEngine`)
from config and injects the `githubapi` client. Provider SDKs stay out via `setup` helpers. Tests are
deterministic glue only — no assertions on model output.
