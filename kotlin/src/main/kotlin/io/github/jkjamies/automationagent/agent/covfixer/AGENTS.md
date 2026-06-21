# agent.covfixer

The test-coverage configuration of the [fixflow](../fixflow/AGENTS.md) engine. It triages an agnostic coverage report into source files with
meaningful uncovered logic, then generates language-aware tests for them. Its prompts are entirely
separate from the lint-fixer's; only the deterministic loop is shared.

- `Coverage.kt` — `newEngine(deps)`: builds the `fixflow.Engine` with the coverage `Spec`
  (`agent-coverage-verify` check, `automation-agent-coverage` label, `automation-agent/test-coverage`
  branch).
- `Triage.kt` — `triage`: LLM-normalizes an arbitrary coverage report into `FileWork`.
- `Analyze.kt` — `analyze` = **explore → execute**: a tool-using `fixflow.explore` agent navigates
  the checkout to plan test placement grounded in the repo's real conventions (`parsePlan`), then
  `fixflow.parallelAnalyze` writes one test per file from that plan.
- `resources/prompts/covfixer/` — `triage`, `explore`, `analyze`, `summarize_result`.

Tested with a scripted stub LLM that routes by system-prompt marker (triage / explore-plan /
execute) over a seeded checkout. Never assert on LLM output content.
