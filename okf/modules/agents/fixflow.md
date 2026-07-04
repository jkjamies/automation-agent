---
type: Workflow
title: Fixflow Engine
description: The reusable event-driven fix-loop engine behind the PR-fixing workflows — kickoff, apply, durable suspend across the CI wait, CI resume, loop or finish.
resource: go/internal/agent/fixflow
tags: [fix-loop, suspend-resume, ci, workflow-graph]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Fixflow Engine

The reusable engine behind the PR-fixing agents ([lint-fixer](/modules/agents/lintfixer.md), [coverage-fixer](/modules/agents/covfixer.md), …). It owns the event-driven fix loop — kickoff → apply → **suspend across the CI wait** → CI resume → loop or finish — plus the apply mechanics. Each concrete agent supplies a `Spec` (its own triage fn, analyze fn, and branch/label/check names) **and its own prompts**; nothing about the LLM prompting is shared here.

## Durable suspend/resume

The CI wait is a real ADK **IsLongRunning** suspend/resume: the `Driver` runs a `fixer` agent that calls `apply_fix` then parks on `await_ci`. Both the ADK session and the parked run are persisted through `SESSION_BACKEND` (`memory` | `sqlite` | `firestore`): the run is recorded in the injected `setup.ParkStore` (`ParkRecord` keyed by a UUID session id, with a `owner/repo#pr` `PRKey` index for CI resume). With a durable backend a process restart resumes in-flight runs; the default `memory` backend stays ephemeral (a restart strands them). Attempts are counted in the park record — **not** from GitHub commits.

A run whose CI never reports is freed two ways: a soft per-run `CITimeout` timer (in-process, lost on restart) and the durable `SweepTimeouts` catch-all (driven by `/internal/sweep`). `ResolveByPRKey`/`Sweep` claim a run atomically (single winner), so a late/duplicate webhook racing the timer/sweep resolves it at most once.

Terminal resolution (`clear`) deletes both the park record and the ADK session (`LongRunDriver.DeleteSession`) so durable backends don't accumulate finished runs.

## Workflow graph

The outer loop is a deterministic workflow graph (`Start → apply_fix → await_ci`, with a conditional `failure` cycle back to `apply_fix` and a shared `conclude` terminal), so retry/stop/timeout policy is all in the `Driver`, not the graph. The substantive LLM work (triage, exploration, code edits) happens inside the `apply_fix` node → `attemptOnce`.

## Flow

```mermaid
flowchart TD
    Spec["Spec{Name, Branch, Label, CheckName, Triage, Analyze, titles}"] --> E["NewEngine(spec, Deps)"]
    K["Kickoff(raw)"] --> KP["ParseKickoff{repo, base, report}"]
    KP --> DK["Driver.Kickoff: run fixer agent"]
    DK --> AF["apply_fix -> attemptOnce: Triage -> Open -> Analyze -> Commit (clone/branch/push/ensure PR)"]
    AF -->|"triage found nothing (ErrNoWork)"| CLN["clean summary (CleanTitle) + clear; no PR, no park (StopWhen concludes)"]
    AF --> AW["await_ci (IsLongRunning)"]
    AW --> PK["ParkStore.Put(prKey=owner/repo#pr, attempts) + CITimeout timer"]
    PK --> SUS(["suspend (durable: survives restart)"])

    SUS -->|"check_run (spec.CheckName) completed"| R["Resume(raw)"]
    R -->|"name != CheckName"| NO["no-op (another engine may handle it)"]
    R --> RES["ParkStore.ResolveByPRKey(prKey) (atomic claim)"]
    RES -->|"late/dup/unknown"| NO2["no-op"]
    RES --> C{conclusion}
    C -->|success| OK["status-aware summary (SuccessTitle) + clear (park + session)"]
    C -->|"failure & attempts >= MaxIter"| HRV["status-aware summary (ReviewTitle) + clear"]
    C -->|"failure & attempts < MaxIter"| RT["resume run -> apply_fix again -> re-park (attempts+1)"]
    RT --> SUS
    TO["CITimeout timer fires"] -.-> TON["onTimeout: claim + summary + clear"]
    SW["/internal/sweep -> SweepTimeouts (durable catch-all)"] -.-> TON
```

## Reference implementation layout (Go port)

The same structure exists in each port; the Go port is the reference.

- `engine.go` — `Engine` + `Spec` + `Deps` + `FileWork`/`FileEdit`/`AnalyzeInput`; `Kickoff`/`Resume` (delegate to the Driver) + `attemptOnce` (one apply attempt).
- `driver.go` — `Driver`: the `apply_fix`/`await_ci`/`conclude` workflow nodes, the `fixer` workflow agent (declarative edges, `await_ci` parks via request-input), and the Kickoff/Resume/onTimeout/`SweepTimeouts` lifecycle over the injected `setup.ParkStore`. Terminal `clear` deletes the park record **and** the ADK session.
- `summary.go` — `buildSummaryText`: the status-aware terminal summary (success / clean / max-iter / timeout framings) enriched with `GH.Compare` (base...branch diff) + the park record. The clean framing is a workflow-prefixed fun line rotated deterministically by repo.
- `applyfix.go` — clone → branch (new/existing) → commit → push → ensure labeled PR.
- `analyze.go` — `ParallelAnalyze`: one ADK parallel agent per `FileWork`, distinct state keys so they never collide.
- `envelope.go` — the trusted `{repo, base, report}` kickoff envelope.
- `util.go` — `Engine.Label()`, `ExtractJSONArray/Object`, `StripFences`.

The generic pause/resume plumbing (`LongRunDriver`) lives in the [setup platform package](/modules/agents/setup.md) — it touches the provider SDK (`genai`), which the architecture rules confine to `setup`.

## Multi-engine dispatch

Multiple engines can each be handed a `check_run` event; only the one whose `CheckName` matches acts.

## Testing

Tested with fake triage/analyze + a local seed repo + fakes, driving the real ADK runner through park/resume.
