"""The in-house PR code-review workflow (a CodeRabbit-style advisory reviewer).

It reacts to GitHub ``pull_request`` events (routed as :attr:`ingest.Kind.REVIEW`) and posts
per-category sub-agent findings, a count-based scorecard, inline comments with suggestions, and an
advisory ``agent-review`` check. Comment-only; it never opens PRs.
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
