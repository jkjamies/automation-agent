# agent.fixflow

The reusable engine behind the PR-fixing agents (lint-fixer, coverage-fixer, …). It owns the event-driven fix loop — kickoff → apply → **suspend
across the CI wait** → CI resume → loop or finish — plus the apply mechanics. Each concrete agent
supplies a `Spec` (its triage fn, analyze fn, and branch/label/check names) **and its own prompts**.

The CI wait is a real ADK **resumability** suspend/resume on an **in-memory** session: the `Driver`
runs a `fixer` agent that calls `apply_fix` then parks on `await_ci`. The parked run is tracked in
an in-memory **registry** (keyed by `owner/repo#pr`); there is no durable store and no reconciler,
so a process restart strands in-flight runs (an accepted trade). Attempts are counted in the
registry — **not** from GitHub commits. A per-run `ciTimeout` timer frees a run whose CI never
reports. The outer loop is driven by a deterministic `setup.newSequencerModel`, so retry/stop/
timeout policy is all in the `Driver`, not the model.

## Files

- `Engine.kt` — `Engine` + `Spec` + `Deps` + `FileWork`/`FileEdit`/`AnalyzeInput`; `kickoff`/
  `resume` (delegate to the Driver) + `attemptOnce` (one apply attempt).
- `Driver.kt` — `Driver`: the `apply_fix`/`await_ci` `BaseTool`s, the `fixer` agent (on a
  deterministic sequencer model), and the kickoff/resume/onTimeout lifecycle over the registry. The
  apply_fix tool reads its session id from `ToolContext.invocationContext.session.key.id` to look up
  the Driver-owned run params.
- `Registry.kt` — in-memory parked-run registry; atomic `resolve` (one of webhook/timer wins). Per-
  run timeouts are coroutine jobs on the Driver's scope.
- `ApplyFix.kt` — clone → branch (new/existing) → commit → push → ensure labeled PR.
- `Analyze.kt` — `parallelAnalyze`: one ADK parallel agent per `FileWork`, distinct state keys so
  they never collide.
- `Explore.kt` — a tool-using LLM agent that navigates the checkout (read_file/list_dir).
- `Tools.kt` / `Files.kt` — the read-only repo `BaseTool`s and path-safe file access (`safeJoin`
  rejects traversal/absolute paths).
- `Envelope.kt` — the trusted `{repo, base, report}` kickoff envelope.
- `Util.kt` — `extractJsonArray`/`extractJsonObject`/`stripFences`.

The generic suspend/resume plumbing (`LongRunDriver`, `newSequencerModel`) lives in `agent.setup`.

Multiple engines can each be handed a `check_run` event; only the one whose `checkName` matches
acts. Tested with fake triage/analyze + a JGit-seeded local repo + fakes, driving the real ADK
runner through park/resume.
