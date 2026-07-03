"""Generic workflow pause/resume plumbing.

A :class:`LongRunDriver` that runs a parking workflow through one cycle — until it pauses
on a request-input interrupt (or finishes) — then resumes it with the real result. All
domain policy (what to apply, whether to retry, how long to wait) lives in the caller.
Kept in ``setup`` because it touches genai (confined here by the arch tests).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from google.adk.apps import App, ResumabilityConfig
from google.adk.runners import Runner
from google.adk.sessions import BaseSessionService, InMemorySessionService
from google.genai import types

from automation_agent.agent.setup.events import content_text
from automation_agent.agent.setup.runner import STREAMING_RUN_CONFIG

# NODE_OUTPUT_KEY is the reserved key a workflow node sets in its dict-typed event output
# to name itself. Node events carry no engine-level node identity for top-level static
# nodes, so drives attribute outputs to nodes by this self-describing marker instead of
# relying on event order.
NODE_OUTPUT_KEY = "node"

# The engine routes a human/webhook reply back to a paused node via a FunctionResponse
# whose call name is this well-known value (the request-input pause emits the matching
# FunctionCall). The SDK does not export the constant publicly, so it is pinned here.
_REQUEST_INPUT_CALL_NAME = "adk_request_input"


@dataclass
class DriveResult:
    """The outcome of driving a parking workflow through one cycle."""

    # parked_call_id is the interrupt id of the request-input pause the run suspended on,
    # or "" when the run finished instead of parking. Feeding it back to resume routes the
    # real result to the waiting node.
    parked_call_id: str = ""
    # node_outputs are the dict-typed event outputs emitted this cycle, in order. Nodes
    # tag their output with NODE_OUTPUT_KEY; node_output selects by that tag.
    node_outputs: list[dict[str, Any]] = field(default_factory=list)
    # final is the concatenated text of the run's non-partial responses.
    final: str = ""

    def node_output(self, node: str) -> dict[str, Any] | None:
        """The most recent output this cycle tagged with the given node name, or None."""
        for out in reversed(self.node_outputs):
            if out.get(NODE_OUTPUT_KEY) == node:
                return out
        return None


class LongRunDriver:
    """Drives a parking workflow through the engine's pause/resume on a single session.

    All domain policy (what to apply, whether to retry, how long to wait) lives in the
    caller; this type only knows how to run-to-park and resume-with-a-result.
    """

    def __init__(
        self,
        app_name: str,
        user_id: str,
        root: Any,
        session_service: BaseSessionService | None = None,
    ) -> None:
        app = App(
            name=app_name,
            root_agent=root,
            resumability_config=ResumabilityConfig(is_resumable=True),
        )
        # A durable session_service (sqlite/firestore) makes a parked run survive a process
        # restart (its paused state is reconstructed from persisted session events); the
        # default in-memory one keeps today's behavior (a restart strands it).
        self._session_service = session_service or InMemorySessionService()
        self._runner = Runner(
            app=app, session_service=self._session_service, auto_create_session=True
        )
        self._app_name = app_name
        self._user_id = user_id

    async def delete_session(self, session_id: str) -> None:
        """Remove a session's stored history. Terminal cleanup calls this so a durable
        backend (sqlite/firestore) does not accumulate completed sessions; on the in-memory
        backend it just frees the map entry. Deleting a missing session is a no-op."""
        await self._session_service.delete_session(
            app_name=self._app_name, user_id=self._user_id, session_id=session_id
        )

    async def start(self, session_id: str, text: str) -> DriveResult:
        """Seed a fresh invocation on ``session_id`` and drive until the workflow pauses
        on a request-input interrupt or finishes."""
        await self._ensure_session(session_id)
        msg = types.Content(role="user", parts=[types.Part.from_text(text=text)])
        return await self._drive(session_id, msg)

    async def resume(self, session_id: str, call_id: str, response: dict[str, Any]) -> DriveResult:
        """Feed the real result for a parked request-input pause (``call_id`` is the
        interrupt id a prior drive parked on) back into ``session_id`` and drive until the
        workflow re-parks or finishes. The caller is the gate against stale resumes —
        claim the run before resuming it."""
        msg = types.Content(
            role="user",
            parts=[
                types.Part(
                    function_response=types.FunctionResponse(
                        id=call_id, name=_REQUEST_INPUT_CALL_NAME, response=response
                    )
                )
            ],
        )
        return await self._drive(session_id, msg)

    async def _ensure_session(self, session_id: str) -> None:
        existing = await self._session_service.get_session(
            app_name=self._app_name, user_id=self._user_id, session_id=session_id
        )
        if existing is None:
            await self._session_service.create_session(
                app_name=self._app_name, user_id=self._user_id, session_id=session_id
            )

    async def _drive(self, session_id: str, msg: types.Content) -> DriveResult:
        res = DriveResult()
        parts: list[str] = []
        async for ev in self._runner.run_async(
            user_id=self._user_id,
            session_id=session_id,
            new_message=msg,
            run_config=STREAMING_RUN_CONFIG,
        ):
            if ev.long_running_tool_ids:
                res.parked_call_id = next(iter(ev.long_running_tool_ids))
            if isinstance(ev.output, dict):
                res.node_outputs.append(ev.output)
            if ev.content is not None and not ev.partial:
                parts.append(content_text(ev.content))
        res.final = "".join(parts)
        return res
