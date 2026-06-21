"""A single tool-using agent that navigates the checkout to ground a plan.

Port of ``fixflow/explore.go``. The model decides what to read (via read_file/list_dir);
no Python code pre-selects files. Workflows use this to ground a plan (e.g. where tests
belong) in the repo's actual conventions rather than a hardcoded rule.
"""

from __future__ import annotations

from typing import Any, cast

from google.adk.agents import LlmAgent
from google.adk.models import BaseLlm

from automation_agent.agent import setup
from automation_agent.agent.fixflow.tools import repo_tools


async def explore(
    llm: BaseLlm, repo_dir: str, instruction: str, input: str
) -> str:
    """Run a tool-using agent rooted at ``repo_dir`` and return its final text answer."""
    agent = LlmAgent(
        name="explorer",
        description="Examines the repository to ground a plan in its real conventions.",
        model=llm,
        instruction=instruction,
        tools=cast("list[Any]", repo_tools(repo_dir)),
    )
    runner = setup.new_runner("fix-explore", agent)
    return await setup.drive_text(runner, "system", "explore", input)
