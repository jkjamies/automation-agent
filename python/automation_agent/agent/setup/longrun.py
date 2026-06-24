"""Generic ADK long-running suspend/resume plumbing.

A :class:`LongRunDriver` that runs an agent until it parks on a long-running tool
call (or finishes), then resumes it with
the real result; and a :class:`Sequencer` model that deterministically emits a fixed
Action -> Wait tool sequence so all retry/stop/timeout policy lives in the caller, not
the model. Kept in ``setup`` because it touches genai (confined here by the arch tests).
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from google.adk.agents import BaseAgent
from google.adk.apps import App, ResumabilityConfig
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.adk.runners import Runner
from google.adk.sessions import BaseSessionService, InMemorySessionService
from google.genai import types

from automation_agent.agent.setup.events import assistant_text, content_text


@dataclass
class DriveResult:
    """The outcome of driving a long-running agent through one cycle."""

    # parked_call_id is the id of the long-running call the agent suspended on, or ""
    # when the run finished instead of parking.
    parked_call_id: str = ""
    # tool_responses maps each tool name to its most recent response this cycle. A tool
    # whose handler raised surfaces here under an "error" key.
    tool_responses: dict[str, dict[str, Any]] = field(default_factory=dict)
    # final is the concatenated text of the agent's non-partial responses.
    final: str = ""


class LongRunDriver:
    """Drives an agent through ADK suspend/resume on a single in-memory session.

    All domain policy (what to apply, whether to retry, how long to wait) lives in the
    caller; this type only knows how to run-to-park and resume-with-a-result.
    """

    def __init__(
        self,
        app_name: str,
        user_id: str,
        root: BaseAgent,
        session_service: BaseSessionService | None = None,
    ) -> None:
        app = App(
            name=app_name,
            root_agent=root,
            resumability_config=ResumabilityConfig(is_resumable=True),
        )
        # A durable session_service (sqlite/firestore) makes a parked run survive a process
        # restart; the default in-memory one keeps today's behavior (a restart strands it).
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
        """Seed a fresh invocation on ``session_id`` and drive until the agent parks
        on a long-running tool or finishes."""
        await self._ensure_session(session_id)
        msg = types.Content(role="user", parts=[types.Part.from_text(text=text)])
        return await self._drive(session_id, msg)

    async def resume(
        self, session_id: str, call_id: str, tool_name: str, response: dict[str, Any]
    ) -> DriveResult:
        """Feed the real result for a parked long-running call back into ``session_id``
        and drive until the agent re-parks or finishes."""
        msg = types.Content(
            role="user",
            parts=[
                types.Part(
                    function_response=types.FunctionResponse(
                        id=call_id, name=tool_name, response=response
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
            user_id=self._user_id, session_id=session_id, new_message=msg
        ):
            if ev.long_running_tool_ids:
                res.parked_call_id = next(iter(ev.long_running_tool_ids))
            if ev.content is None:
                continue
            for p in ev.content.parts or []:
                if p.function_response is not None and p.function_response.name:
                    res.tool_responses[p.function_response.name] = dict(
                        p.function_response.response or {}
                    )
            if not ev.partial:
                parts.append(content_text(ev.content))
        res.final = "".join(parts)
        return res


class Sequencer(BaseLlm):
    """A deterministic ``BaseLlm`` that emits a fixed Action -> Wait tool sequence.

    Call ``action`` (a normal tool), then ``wait`` (a long-running tool that suspends
    the run). When the run resumes with ``wait``'s real result, ``retry_when`` decides
    whether to loop (call ``action`` again) or conclude. It carries no policy of its
    own: the caller owns retry/stop/timeout and only resumes a parked run when it wants
    another attempt.
    """

    action: str
    wait: str
    retry_when: Callable[[dict[str, Any]], bool] | None = None
    model_config = {"arbitrary_types_allowed": True}

    def __init__(
        self,
        action: str,
        wait: str,
        retry_when: Callable[[dict[str, Any]], bool] | None = None,
    ) -> None:
        super().__init__(  # type: ignore[call-arg]
            model=f"sequencer:{action}+{wait}",
            action=action,
            wait=wait,
            retry_when=retry_when,
        )

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False):  # type: ignore[override]
        yield self._decide(llm_request.contents or [])

    def _decide(self, contents: list[types.Content]) -> LlmResponse:
        """Choose the next step from the most recent function response in history:

        * none yet                  -> call Action
        * Action returned an error  -> conclude (nothing to wait on)
        * Action returned a result  -> call Wait, forwarding the result as its args
        * Wait result, retry_when   -> call Action again
        * Wait result, otherwise    -> conclude
        """
        last = _last_function_response(contents)
        if last is None:
            return self._call(self.action, None, contents)
        if last.name == self.action:
            resp = dict(last.response or {})
            if "error" in resp:
                return _sequencer_text(f"{self.action} failed: {resp['error']}")
            return self._call(self.wait, resp, contents)
        if last.name == self.wait:
            if self.retry_when is not None and self.retry_when(dict(last.response or {})):
                return self._call(self.action, None, contents)
            return _sequencer_text("done")
        return _sequencer_text("done")

    def _call(
        self, name: str, args: dict[str, Any] | None, contents: list[types.Content]
    ) -> LlmResponse:
        args = args or {}
        # Unique id per call so the flow correlates each long-running park independently
        # across retries within one session.
        call_id = f"{name}_{_count_function_calls(contents, name) + 1}"
        fc = types.FunctionCall(id=call_id, name=name, args=args)
        return LlmResponse(
            content=types.Content(role="model", parts=[types.Part(function_call=fc)]),
            turn_complete=True,
        )


def _sequencer_text(text: str) -> LlmResponse:
    return LlmResponse(content=assistant_text(text), turn_complete=True)


def _last_function_response(
    contents: list[types.Content],
) -> types.FunctionResponse | None:
    last: types.FunctionResponse | None = None
    for c in contents:
        if c is None:
            continue
        for p in c.parts or []:
            if p.function_response is not None:
                last = p.function_response
    return last


def _count_function_calls(contents: list[types.Content], name: str) -> int:
    n = 0
    for c in contents:
        if c is None:
            continue
        for p in c.parts or []:
            if p.function_call is not None and p.function_call.name == name:
                n += 1
    return n
