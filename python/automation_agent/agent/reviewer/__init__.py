"""The in-house PR code-review workflow (a CodeRabbit-style advisory reviewer).

It reacts to GitHub ``pull_request`` events (routed as :attr:`ingest.Kind.REVIEW`), gates the PR
(size/draft/exclude filters), runs per-category sub-agents, and produces a count-based scorecard.
Publishing the results (inline comments, the marker summary, and the advisory ``agent-review``
check) is a follow-up. Comment-only; it never opens PRs. See
``specs/20260625-pr-code-review-agent.md``.
"""

from __future__ import annotations

from automation_agent.agent.reviewer.enqueue import enqueue_options
from automation_agent.agent.reviewer.reviewer import (
    Decision,
    DecisionKind,
    Deps,
    Engine,
    new_engine,
)

__all__ = [
    "Decision",
    "DecisionKind",
    "Deps",
    "Engine",
    "enqueue_options",
    "new_engine",
]
