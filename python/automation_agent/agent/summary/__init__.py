"""Public API for the summary workflow package (port of ``internal/agent/summary``)."""

from __future__ import annotations

from automation_agent.agent.summary.agents_setup import Deps, build_summary_agent
from automation_agent.agent.summary.summary import (
    DIGEST_KEY,
    STATE_PREFIX,
    CommitLister,
    build_instruction,
    default_now,
    first_line,
    format_commits,
    new_fetch_agent,
    new_notify_agent,
    safe_name,
    short_sha,
    split_repo,
    summary_instruction,
)

__all__ = [
    "DIGEST_KEY",
    "STATE_PREFIX",
    "CommitLister",
    "Deps",
    "build_instruction",
    "build_summary_agent",
    "default_now",
    "first_line",
    "format_commits",
    "new_fetch_agent",
    "new_notify_agent",
    "safe_name",
    "short_sha",
    "split_repo",
    "summary_instruction",
]
