# automation_agent/agent/fixflow

The reusable engine behind the PR-fixing agents (lint-fixer, coverage-fixer, …). It
owns the event-driven fix loop — kickoff -> apply -> **suspend across the CI wait** -> CI
resume -> loop or finish — plus the apply mechanics. Each concrete agent supplies a
`Spec` (its own triage fn, analyze fn, and branch/label/check names) **and its own
prompts**; nothing about the LLM prompting is shared here.

The CI wait is a real ADK **IsLongRunning** suspend/resume on an **in-memory** session:
the `Driver` runs a `fixer` agent that calls `apply_fix` then parks on `await_ci`. The
parked run is tracked in an in-memory **registry** (keyed by `owner/repo#pr`); there is
no durable store and no reconciler, so a process restart strands in-flight runs (an
accepted trade). Attempts are counted in the registry — **not** from GitHub commits.
A per-run `ci_timeout` timer frees a run whose CI never reports.

The outer loop is driven by a deterministic `setup.Sequencer` (a class extending
`BaseLlm` that emits a fixed apply->await sequence), so retry/stop/timeout policy is all
in the `Driver`, not the model. The substantive LLM work (triage, exploration, code edits) happens inside
`apply_fix` -> `attempt_once`.

## Flow

```mermaid
flowchart TD
    Spec["Spec{name, branch, label, check_name, triage, analyze, titles}"] --> E["new_engine(spec, Deps)"]
    K["kickoff(raw)"] --> KP["parse_kickoff{repo, base, report}"]
    KP --> DK["Driver.kickoff: run fixer agent"]
    DK --> AF["apply_fix -> attempt_once: triage -> open -> analyze -> commit (clone/branch/push/ensure PR)"]
    AF --> AW["await_ci (IsLongRunning)"]
    AW --> PK["registry.park(owner/repo#pr, attempts) + ci_timeout timer"]
    PK --> SUS(["suspend"])

    SUS -->|"check_run (spec.check_name) completed"| R["resume(raw)"]
    R -->|"name != check_name"| NO["no-op (another engine may handle it)"]
    R --> RES["registry.resolve(pr_key)"]
    RES -->|"late/dup/unknown"| NO2["no-op"]
    RES --> C{conclusion}
    C -->|success| OK["notify success_title + free run"]
    C -->|failure & attempts >= max_iter| HRV["notify review_title + free run"]
    C -->|failure & attempts < max_iter| RT["resume run -> apply_fix again -> re-park (attempts+1)"]
    RT --> SUS
    TO["ci_timeout fires"] -.-> TON["on_timeout: free run + notify review_title"]
```

## Files

- `engine.py` — `Engine` + `Spec` + `Deps` + `FileWork`/`FileEdit`/`AnalyzeInput`;
  `kickoff`/`resume` (delegate to the Driver) + `attempt_once` (one apply attempt).
- `driver.py` — `Driver`: the `apply_fix`/`await_ci` tools, the `fixer` agent (on a
  deterministic sequencer model), and the kickoff/resume/on_timeout lifecycle over the
  registry.
- `registry.py` — in-memory parked-run registry; atomic `resolve` (one of webhook/timer
  wins).
- `applyfix.py` — clone -> branch (new/existing) -> commit -> push -> ensure labeled PR.
- `analyze.py` — `parallel_analyze`: one ADK parallel agent per `FileWork`, distinct
  state keys so they never collide.
- `envelope.py` — the trusted `{repo, base, report}` kickoff envelope.
- `util.py` — `Engine.label()`, `extract_json_array/object`, `strip_fences`.

The generic suspend/resume plumbing (`LongRunDriver`, the `Sequencer` class) lives in
`automation_agent/agent/setup` (it touches `genai`, which arch confines to `setup`).

Multiple engines can each be handed a `check_run` event; only the one whose
`check_name` matches acts. Tested with fake triage/analyze + a local seed repo + fakes,
driving the real ADK runner through park/resume.
