/**
 * The test-coverage configuration of the fixflow engine.
 *
 * Triages an agnostic coverage report into source files with meaningful uncovered logic,
 * then generates tests for them grounded in the repo's real conventions. Its prompts are
 * entirely separate from the lint-fixer's; only the deterministic loop is shared
 * (fixflow).
 */
import { type Deps, type Engine, newEngine } from '../fixflow/index';
import { analyze } from './analyze';
import { triage } from './triage';

/** Build the coverage-fixer engine. */
export function newCoverageEngine(d: Deps): Engine {
  return newEngine(
    {
      name: 'coverage',
      branch: 'automation-agent/test-coverage',
      checkName: 'agent-coverage-verify',
      commitMessage: 'automation-agent: add test coverage',
      prTitle: 'automation-agent: add test coverage',
      successTitle: 'Coverage fix succeeded ✅',
      reviewTitle: 'Coverage fix needs human review ⚠️',
      triage,
      analyze,
    },
    d,
  );
}
