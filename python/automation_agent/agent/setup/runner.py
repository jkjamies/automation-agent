"""In-memory runner helpers for driving workflow agents locally and in tests.

Mirrors Go's ``setup/runner.go``. Go's synchronous ``iter.Seq2`` event loop becomes
an ``async for`` over ADK's ``Runner.run_async``; Go's ``context.Context`` plumbing
maps to Python async cancellation and is therefore dropped from these signatures.
"""

from __future__ import annotations

from typing import Any

from google.adk.agents import BaseAgent
from google.adk.runners import Runner
from google.adk.sessions import InMemorySessionService

from automation_agent.agent.setup.events import content_text, user_text


def new_runner(app_name: str, root: BaseAgent) -> Runner:
    """Build an in-memory runner rooted at ``root``."""
    return Runner(
        app_name=app_name,
        agent=root,
        session_service=InMemorySessionService(),
        auto_create_session=True,
    )


async def drive(runner: Runner, user_id: str, session_id: str, text: str) -> None:
    """Run the agent for a single input, draining events.

    Side-effecting agents (e.g. a notifier) perform their work as they run.
    """
    async for _ in runner.run_async(
        user_id=user_id, session_id=session_id, new_message=user_text(text)
    ):
        pass


async def drive_text(runner: Runner, user_id: str, session_id: str, text: str) -> str:
    """Run the agent and return the concatenated text of its non-partial responses.

    For a tool-using agent this is the final answer after any tool calls
    (intermediate function-call/response events carry no text).
    """
    parts: list[str] = []
    async for ev in runner.run_async(
        user_id=user_id, session_id=session_id, new_message=user_text(text)
    ):
        if ev.content is not None and not ev.partial:
            parts.append(content_text(ev.content))
    return "".join(parts)


async def drive_collect_state(
    runner: Runner, user_id: str, session_id: str, text: str
) -> dict[str, Any]:
    """Run the agent and accumulate every emitted state delta into one map.

    Useful for fan-out workflows where parallel sub-agents each write a distinct
    state key the caller needs to read back.
    """
    state: dict[str, Any] = {}
    async for ev in runner.run_async(
        user_id=user_id, session_id=session_id, new_message=user_text(text)
    ):
        if ev.actions and ev.actions.state_delta:
            state.update(ev.actions.state_delta)
    return state
