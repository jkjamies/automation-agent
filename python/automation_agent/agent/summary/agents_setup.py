"""Builds the summary workflow agent (port of ``summary/agents_setup.go``).

    Sequential[ Parallel[fetch×N] -> summarize(LLM) -> notify ]

Fetchers write per-repo commit data to state; the summarizer reads it via its instruction
provider and writes the digest; the notifier posts it.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime, timedelta

from google.adk.agents import BaseAgent, LlmAgent, ParallelAgent, SequentialAgent
from google.adk.models import BaseLlm

from automation_agent.agent import setup
from automation_agent.agent.summary.summary import (
    DIGEST_KEY,
    CommitLister,
    default_now,
    new_fetch_agent,
    new_notify_agent,
    summary_instruction,
)
from automation_agent.notify import Notifier

_prompts = setup.Prompts("automation_agent.agent.summary")


@dataclass
class Deps:
    """Injected dependencies for the summary workflow."""

    llm: BaseLlm
    gh: CommitLister
    notify: Notifier
    repos: list[str]  # owner/repo entries; one parallel fetcher each
    window: timedelta = timedelta(hours=24)  # commit window; defaults to 24h
    now: Callable[[], datetime] = field(default=default_now)  # injectable clock


def build_summary_agent(d: Deps) -> SequentialAgent:
    """Wire the summary workflow.

    Raises:
        ValueError: if no repos are configured or a required dependency is missing.
    """
    if not d.repos:
        raise ValueError("summary: at least one repo is required")
    if d.llm is None or d.gh is None or d.notify is None:
        raise ValueError("summary: llm, gh and notify are required")

    now = d.now if d.now is not None else default_now
    window = d.window or timedelta(hours=24)

    fetchers: list[BaseAgent] = [
        new_fetch_agent(repo, d.gh, window, now) for repo in d.repos
    ]
    parallel = ParallelAgent(
        name="fetch_all",
        description="Fetches recent commits for all configured repositories",
        sub_agents=fetchers,
    )

    summarizer = LlmAgent(
        name="summarizer",
        description="Summarizes recent commits into a digest",
        model=d.llm,
        instruction=summary_instruction(_prompts.must_get("summarize")),
        output_key=DIGEST_KEY,
    )

    return SequentialAgent(
        name="summary_workflow",
        description="Daily commit digest workflow",
        sub_agents=[parallel, summarizer, new_notify_agent(d.notify)],
    )
