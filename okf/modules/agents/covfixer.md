---
type: Workflow
title: Coverage Fixer Workflow
description: The test-coverage configuration of the fixflow engine — triages a coverage report, plans test placement by exploring the repo's real conventions, and generates tests on a CI-verified loop.
resource: go/internal/agent/covfixer
tags: [coverage, test-generation, ci-loop]
sensitivity: internal
bundle: automation-agent
status: stable
generated: { by: human:jkjamies, at: 2026-07-29T00:00:00Z }
---

# Coverage Fixer Workflow

The **test-coverage** configuration of the [fixflow engine](/modules/agents/fixflow.md). It triages an agnostic coverage report into source files with meaningful uncovered logic, then generates tests for them. Its prompts are entirely its own — separate from the [lint-fixer](/modules/agents/lintfixer.md)'s — and only the deterministic loop is shared (fixflow).

**Test placement is never derived from a hardcoded rule.** The engine checks the repo out once; an **explorer** examines the repo's *actual* existing tests to plan where each test belongs and which framework to use, and parallel **executors** write the tests from that grounded plan. The explorer is a tool-using LLM agent (`fixflow.Explore`): the model itself navigates the checkout via `read_file`/`list_dir` to gather real test-convention evidence — no engine code pre-selects the files it reads.

## Flow

```mermaid
flowchart TD
    K["KindCoverage -> Engine.Kickoff"] --> Open["openCheckout: clone + checkout (shared)"]
    Open --> T["triage(LLM, report) -> []FileWork (files + uncovered)"]
    T --> EX["explore: fixflow.Explore (tool-using agent, read_file/list_dir)"]
    EX -->|"navigates the real checkout"| Plan["LLM plan (prompts/explore.md) -> [{source, test_path, framework, notes}]"]
    Plan --> Exec["execute: fixflow.ParallelAnalyze (one per file)"]
    Exec --> Read["fixflow.ReadFile(checkout, source)"]
    Exec -->|"prompts/analyze.md: write test from brief"| Gen["GenerateText -> test content"]
    Gen --> FE["FileEdit{plan.test_path, content}"]
    FE --> Commit["commitEdits -> branch automation-agent/test-coverage, label automation-agent"]
    Commit --> Loop["suspend -> agent-coverage-verify (runs tests + coverage) -> resume: success / retry-with-feedback / needs-review"]
```

## Verification loop

Generated tests that don't compile or don't raise coverage are rejected by the `agent-coverage-verify` check and retried with the CI output as feedback — the same loop as the [lint-fixer](/modules/agents/lintfixer.md).

## Implementation layout

- `coverage.go` — `NewEngine(Deps)`: the coverage `Spec` (branch/check + titles); the PR label is one service-wide setting on `fixflow.Deps`, not per-Spec.
- `triage.go` — coverage report → `[]fixflow.FileWork` (files + uncovered regions).
- `analyze.go` — `explore` (a tool-using agent via `fixflow.Explore` grounds a per-file plan in the repo's real conventions) then `execute` (parallel test generation, reading each source with `fixflow.ReadFile`).
- `prompts/{triage,explore,analyze}.md`. The terminal chat summary is assembled deterministically in Go (`fixflow.buildSummaryText`), not by a prompt.

## Testing

Tested with a scripted LLM + a temp checkout; live behavior is gated behind an opt-in environment flag.
