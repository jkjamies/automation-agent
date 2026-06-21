# automation_agent/agent/covfixer

The **test-coverage** configuration of the `fixflow` engine. It triages an agnostic
coverage report into source files with meaningful uncovered logic, then generates
tests for them. Its prompts are entirely its own — separate from the lint-fixer's —
and only the deterministic loop is shared (`fixflow`).

**Test placement is never derived from a hardcoded rule.** The engine checks the repo
out once; an **explorer** examines the repo's *actual* existing tests to plan where
each test belongs and which framework to use, and parallel **executors** write the
tests from that grounded plan. (The explorer is code-driven — it walks the checkout
for real test-convention evidence and hands it to the model — because the local
Ollama/Gemma path doesn't support function-calling tools; a Gemini/cloud path could
use live tool exploration instead.)

## Flow

```mermaid
flowchart TD
    K["KindCoverage -> Engine.kickoff"] --> Open["fixflow.open: clone + checkout (shared)"]
    Open --> T["triage(LLM, report) -> [FileWork] (files + uncovered)"]
    T --> EX["explore: gather_test_conventions(checkout) -> evidence"]
    EX -->|"existing tests + example"| Plan["LLM plan (prompts/explore.md) -> [{source, test_path, framework, notes}]"]
    Plan --> Exec["execute: fixflow.parallel_analyze (one per file)"]
    Exec --> Read["fixflow.read_file(checkout, source)"]
    Exec -->|"prompts/analyze.md: write test from brief"| Gen["generate -> test content"]
    Gen --> FE["FileEdit{plan.test_path, content}"]
    FE --> Commit["fixflow.commit -> branch automation-agent/test-coverage, label automation-agent-coverage"]
    Commit --> Loop["suspend -> agent-coverage-verify (runs tests + coverage) -> resume: success / retry-with-feedback / needs-review"]
```

## Files

- `coverage.py` — `new_engine(Deps)`: the coverage `Spec` (branch/label/check + titles).
- `triage.py` — coverage report -> `[fixflow.FileWork]` (files + uncovered regions).
- `analyze.py` — `explore` (gather real conventions + plan) then `execute` (parallel
  test generation); `gather_test_conventions`/`find_test_files` walk the checkout.
- `prompts/{triage,explore,analyze,summarize_result}.md`.

Generated tests that don't compile or don't raise coverage are rejected by the
`agent-coverage-verify` check and retried with the CI output as feedback — same loop
as the lint-fixer. Tested with a scripted LLM + a temp checkout; live behavior gated
behind `OLLAMA_LIVE`.
