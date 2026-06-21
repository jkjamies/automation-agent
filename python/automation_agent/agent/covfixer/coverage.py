"""The test-coverage configuration of the fixflow engine.

Port of ``covfixer/coverage.go``. Triages an agnostic coverage report into source files
with meaningful uncovered logic, then generates language-aware tests for them. Its
prompts are entirely separate from the lint-fixer's; only the deterministic loop is
shared (fixflow).
"""

from __future__ import annotations

from automation_agent.agent.covfixer.analyze import analyze
from automation_agent.agent.covfixer.triage import triage
from automation_agent.agent.fixflow import Deps, Engine, Spec, new_engine


def new_coverage_engine(d: Deps) -> Engine:
    """Build the coverage-fixer engine."""
    return new_engine(
        Spec(
            name="coverage",
            branch="automation-agent/test-coverage",
            label="automation-agent-coverage",
            check_name="agent-coverage-verify",
            commit_message="automation-agent: add test coverage",
            pr_title="automation-agent: add test coverage",
            success_title="Coverage fix succeeded ✅",
            review_title="Coverage fix needs human review ⚠️",
            triage=triage,
            analyze=analyze,
        ),
        d,
    )
