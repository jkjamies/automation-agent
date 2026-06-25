"""The lint-remediation configuration of the fixflow engine.

Supplies the triage step (normalize a linter report) and
the analyze step (rewrite the affected source files), plus its branch/label/check
identity. The loop itself lives in :mod:`automation_agent.agent.fixflow`.
"""

from __future__ import annotations

from automation_agent.agent.fixflow import Deps, Engine, Spec, new_engine
from automation_agent.agent.lintfixer.analyze import analyze
from automation_agent.agent.lintfixer.triage import triage


def new_lint_engine(d: Deps) -> Engine:
    """Build the lint-fixer engine."""
    return new_engine(
        Spec(
            name="lint",
            branch="automation-agent/lint-fix",
            check_name="agent-lint-verify",
            commit_message="automation-agent: fix lint problems",
            pr_title="automation-agent: fix lint problems",
            success_title="Lint fix succeeded ✅",
            review_title="Lint fix needs human review ⚠️",
            triage=triage,
            analyze=analyze,
        ),
        d,
    )
