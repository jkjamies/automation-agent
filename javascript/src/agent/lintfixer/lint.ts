/**
 * The lint-remediation configuration of the fixflow engine.
 *
 * Supplies the triage step (normalize a linter report) and the analyze step (rewrite the
 * affected source files), plus its branch/label/check identity. The loop itself lives in
 * the fixflow engine.
 */
import { type Deps, type Engine, newEngine } from '../fixflow/index';
import { analyze } from './analyze';
import { triage } from './triage';

/** Build the lint-fixer engine. */
export function newLintEngine(d: Deps): Engine {
  return newEngine(
    {
      name: 'lint',
      branch: 'automation-agent/lint-fix',
      checkName: 'agent-lint-verify',
      commitMessage: 'automation-agent: fix lint problems',
      prTitle: 'automation-agent: fix lint problems',
      successTitle: 'Lint fix succeeded ✅',
      reviewTitle: 'Lint fix needs human review ⚠️',
      triage,
      analyze,
    },
    d,
  );
}
