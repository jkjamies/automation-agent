# agent.lintfixer

The lint-remediation configuration of the [fixflow](../fixflow/AGENTS.md) engine. It supplies a triage step (normalize a linter report into
per-file work) and an analyze step (rewrite the affected source files), plus its branch/label/check
identity. The event-driven loop itself lives in `agent.fixflow`.

- `Lint.kt` — `newEngine(deps)`: builds the `fixflow.Engine` with the lint `Spec`
  (`agent-lint-verify` check, `automation-agent` label, `automation-agent/lint-fix` branch).
- `Triage.kt` — `triage`: LLM-normalizes an arbitrary linter report into `FileWork`
  (`parseTriage` extracts the JSON array the model emits).
- `Analyze.kt` — `analyze`: rewrites each affected file via `fixflow.parallelAnalyze`, reading
  current source from the checkout; unreadable files are skipped.
- `resources/prompts/lintfixer/` — `triage`, `analyze`, `summarize_result` (loaded via `Prompts`).

Prompts are entirely separate from the coverage-fixer's; only the deterministic loop is shared.
Tested with a stub LLM (no live model) over the pure triage/analyze steps. Never assert on LLM
output content.
