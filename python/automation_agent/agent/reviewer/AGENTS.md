# agent/reviewer

The in-house **PR code-review** workflow. It reacts to GitHub `pull_request` events and produces a
count-based scorecard from per-category sub-agent findings. It is **comment-only** and never opens
PRs. See `specs/20260625-pr-code-review-agent.md`.

Unlike the lint/coverage fixers, the reviewer is **not** a suspend/resume fix loop: it is mostly
one-shot per `pull_request` event and does not park on `await_ci`. Its long LLM compute runs
**in-request** via the execution transport (`Kind.REVIEW` → `/internal/dispatch`), so CPU stays
allocated on Cloud Run.

Publishing the scored review to the PR — inline comments, the marker summary comment, the advisory
`agent-review` check, reconciliation, and standards-aware steering — is a **follow-up**. This
package currently covers detection and analysis through the scorecard.

## Trigger — native-event kickoff

The reviewer's kickoff is a **native GitHub event** (`pull_request`), not a custom POST route. The
GitHub App delivers it to the single `/webhooks/github` URL, where the handler routes by the
`X-GitHub-Event` header (`pull_request` → `Kind.REVIEW`, `check_run` → `Kind.CI`, anything else →
200 no-dispatch).

## Coalesce-to-latest — debounce + staleness

Rapid pushes to one PR are collapsed so only the latest SHA is reviewed (two parts, because Cloud
Tasks gives no ordering and cannot cancel an in-flight task):

- **Debounce at enqueue** (`enqueue.py`, `enqueue_options`): a `synchronize` review is enqueued
  with `REVIEW_DEBOUNCE` delay under a per-PR-per-window Cloud Tasks dedup name, so a burst of
  pushes collapses to one delayed task. The name carries a time bucket (receipt time floored to
  the debounce window) so a push minutes later doesn't collide with Cloud Tasks' ~1h name
  reservation. `opened`/`reopened`/`ready_for_review` enqueue immediately. Only the Cloud Tasks
  backend honors the hints.
- **Staleness at execution** (`Engine.kickoff` → `_superseded`): before doing the review work, the
  engine fetches the PR's current head SHA and skips if it no longer matches the event's SHA.
  Best-effort — a lookup error proceeds rather than suppressing a real review.

## Data layer — REST-first

The reviewer reads the PR via the GitHub REST API (`githubapi.Client`), over the shared auth
provider (App installation token in production, PAT locally): changed files + patches
(`list_pr_files`), the head SHA (`pull_request_head_sha`), the repo tree (`tree`), and the existing
review comments (`list_review_comments`).

## Intake pipeline

`Engine.decide` runs a deterministic, model-free intake and produces a `Decision` (skip / deny /
review):

1. **Parse** the event (`githubapi.parse_pull_request_event`).
2. **Trigger gate** — only `opened` / `reopened` / `synchronize` / `ready_for_review` proceed.
3. **Skip rules** — draft (unless `ready_for_review`), the agent's own `automation-agent/*`
   branches, the `skip-review` label, and dependency-bot authors.
4. **Fetch** the changed files + patches (`list_pr_files`).
5. **Filter** generated/vendored/lockfile/minified/binary paths (`filter.py`); size is computed on
   the **filtered** set. An empty filtered diff skips.
6. **Size gate** — two-dimensional (`REVIEW_MAX_FILES` **and** `REVIEW_MAX_DIFF_BYTES`): over
   either cap denies (review-or-deny, no degrade tier).

## Review stage (category fan-out → glue → scorecard)

`review.py`: **fan out** one agent per applicable category over the whole filtered diff, in
parallel (ADK `ParallelAgent`). The consolidated set: Safety + Security + Code quality (code tier),
Performance + Accessibility (base tier; accessibility only when UI/markup changed) + an `(other)`
catch-all demoted to nitpick. The **glue/synthesis** pass (code tier, always) adds architectural
alignment, testability, test coverage. Then the deterministic gates in code (`glue.py`): drop below
`REVIEW_MIN_CONFIDENCE`, collapse cross-lens duplicates by fingerprint. The **scorecard**
(`scorecard.py`) is a per-dimension severity histogram → level (🔴 any critical or ≥2 major · 🟡 any
major or ≥3 medium · 🟢 else); overall = critical-cap (any critical in security / runtime safety →
🔴) combined with the worst dimension.

## Structured output

Category agents request `application/json` (valid JSON syntax) and describe the exact findings
schema in their prompt; `parse_findings` recovers **defensively** — it extracts the first decodable
JSON array from the model text, tolerates fences/prose, and treats a malformed body as no findings
(empty = success). Best-effort by design; the narrow single-lens prompts are themselves the
false-positive control.

## Files

- `reviewer.py` — `Deps`, `Engine`, `new_engine`, `Engine.kickoff`, and the `decide` intake +
  skip helpers. Gated by `REVIEW_ENABLED` (default false, the kill switch).
- `filter.py` — the exclude-glob `FileFilter` and the filtered patch-byte total.
- `sizegate.py` — `oversize`, the two-dimensional file-count/diff-byte cap.
- `findings.py` — the `Finding` schema, severity/dimension normalization, `fingerprint`, and the
  defensive `parse_findings`.
- `categories.py` — the consolidated category set + `select_categories` (UI-only gating).
- `scorecard.py` — the count-based `score_findings`.
- `glue.py` — the deterministic verify gate + cross-lens `dedupe`.
- `review.py` — `run_review`: the fan-out drive, glue drive, diff formatting, and instruction
  composition.
- `enqueue.py` — `enqueue_options`: the debounce/coalesce transport hints.
- `agents_setup.py` — the build-agent split: pure ADK wiring (category + glue LLM agents, the
  prompt loader, the JSON `generate_content_config`).
- `prompts/*.md` — one markdown prompt per category and the glue pass.

Wiring: `root` registers `Kind.REVIEW` → `Engine.kickoff`; `cmd` builds the engine (via
`new_engine`) from config and injects the `githubapi` client. Provider SDKs stay out via `setup`
helpers. Tests are deterministic glue only — no assertions on LLM output.
