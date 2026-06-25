"""In-memory runner helpers for driving workflow agents locally and in tests.

A synchronous event loop is expressed here as an ``async for`` over ADK's
``Runner.run_async``; cancellation is handled by Python's async machinery, so there is
no request-context parameter in these signatures.
"""

from __future__ import annotations

from typing import Any

from google.adk.agents import BaseAgent
from google.adk.agents.run_config import RunConfig, StreamingMode
from google.adk.apps import App
from google.adk.runners import Runner
from google.adk.sessions import InMemorySessionService

from automation_agent.agent.setup.events import content_text, user_text

# Stream model output over SSE at every runner call site (the single source of truth for
# this port). A long Ollama generation then streams token-by-token over a long-lived body
# instead of blocking on one buffered response whose header/read timeout would cap the whole
# decode. Every event loop here (and in longrun) filters partial events via ``not ev.partial``,
# so streaming is transparent: tool calls and final text still surface only on the final,
# non-partial events. The default ``RunConfig()`` is ``StreamingMode.NONE`` (no streaming).
STREAMING_RUN_CONFIG = RunConfig(streaming_mode=StreamingMode.SSE)


def new_runner(app_name: str, root: BaseAgent) -> Runner:
    """Build an in-memory runner rooted at ``root``."""
    app = App(name=app_name, root_agent=root)
    return Runner(
        app=app,
        session_service=InMemorySessionService(),
        auto_create_session=True,
    )


async def drive(runner: Runner, user_id: str, session_id: str, text: str) -> None:
    """Run the agent for a single input, draining events.

    Side-effecting agents (e.g. a notifier) perform their work as they run.
    """
    async for _ in runner.run_async(
        user_id=user_id,
        session_id=session_id,
        new_message=user_text(text),
        run_config=STREAMING_RUN_CONFIG,
    ):
        pass


async def drive_text(runner: Runner, user_id: str, session_id: str, text: str) -> str:
    """Run the agent and return the concatenated text of its non-partial responses.

    For a tool-using agent this is the final answer after any tool calls
    (intermediate function-call/response events carry no text).
    """
    parts: list[str] = []
    async for ev in runner.run_async(
        user_id=user_id,
        session_id=session_id,
        new_message=user_text(text),
        run_config=STREAMING_RUN_CONFIG,
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
        user_id=user_id,
        session_id=session_id,
        new_message=user_text(text),
        run_config=STREAMING_RUN_CONFIG,
    ):
        if ev.actions and ev.actions.state_delta:
            state.update(ev.actions.state_delta)
    return state
