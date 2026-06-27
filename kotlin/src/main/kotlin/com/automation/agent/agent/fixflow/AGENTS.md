# agent.fixflow

The reusable engine behind the PR-fixing agents (lint-fixer, coverage-fixer, …). It owns the event-driven fix loop — kickoff → apply → **suspend
across the CI wait** → CI resume → loop or finish — plus the apply mechanics. Each concrete agent
supplies a `Spec` (its triage fn, analyze fn, and branch/label/check names) **and its own prompts**.

The CI wait is a real ADK **resumability** suspend/resume backed by an injected `ParkStore` (from
`agent.setup`, selected by `SESSION_BACKEND`): the `Driver` runs a `fixer` agent that calls
`apply_fix` then parks on `await_ci`. The parked run is persisted as a `ParkRecord` keyed by session
id and indexed by PR key (`owner/repo#pr`). With the `memory` backend a restart drops parked runs;
the `sqlite` and `firestore` backends persist them across restarts. Attempts are counted in the
record — **not** from GitHub commits. A per-run `ciTimeout` soft timer frees a run whose CI never
reports, and a durable reconciler — `sweepTimeouts`, driven by the periodic `/internal/sweep` —
backstops runs whose soft timer was lost to a restart. The store's single-winner claim guarantees
exactly one of {CI webhook, soft timer, sweep} resolves a given run. The outer loop is driven by a
deterministic `setup.newSequencerModel`, so retry/stop/timeout policy is all in the `Driver`, not the
model.

## Files

- `Engine.kt` — `Engine` + `Spec` + `Deps` + `FileWork`/`FileEdit`/`AnalyzeInput`; `kickoff`/
  `resume` (delegate to the Driver) + `attemptOnce` (one apply attempt).
- `Driver.kt` — `Driver`: the `apply_fix`/`await_ci` `BaseTool`s, the `fixer` agent (on a
  deterministic sequencer model), and the kickoff/resume/onTimeout/`sweepTimeouts` lifecycle over the
  injected `ParkStore`. The apply_fix tool reads its session id from
  `ToolContext.invocationContext.session.key.id` to look up the Driver-owned run params. Per-run
  soft timeouts are coroutine jobs on the Driver's scope; the store's atomic claim ensures one of
  webhook/timer/sweep wins.
- `ApplyFix.kt` — clone → branch (new/existing) → commit → push → ensure labeled PR.
- `Analyze.kt` — `parallelAnalyze`: one ADK parallel agent per `FileWork`, distinct state keys so
  they never collide.
- `Explore.kt` — a tool-using LLM agent that navigates the checkout (read_file/list_dir).
- `Tools.kt` / `Files.kt` — the read-only repo `BaseTool`s and path-safe file access (`safeJoin`
  rejects traversal/absolute paths).
- `Envelope.kt` — the trusted `{repo, base, report}` kickoff envelope.
- `Util.kt` — `extractJsonArray`/`extractJsonObject`/`stripFences`.

The generic suspend/resume plumbing (`LongRunDriver`, `newSequencerModel`) and the `ParkStore` +
session-service backends live in `agent.setup`.

When triage finds nothing actionable it throws `NoWorkException`; the apply_fix tool turns that
into a clean result, the sequencer's `stopWhen` concludes without parking, and `finishClean` sends
a positive `cleanTitle` notice (a workflow-prefixed fun line rotated deterministically by repo) —
no PR, no review alarm. Every other error path is unchanged.

Multiple engines can each be handed a `check_run` event; only the one whose `checkName` matches
acts. Tested with fake triage/analyze + a JGit-seeded local repo + fakes, driving the real ADK
runner through park/resume.
