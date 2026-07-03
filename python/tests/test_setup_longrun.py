"""Tests for setup long-run (the LongRunDriver over a parking workflow graph).

The fixture graph is the canonical parking loop the driver is designed for::

    START -> apply -"go_wait"-> await (pause) -"failure"-> apply (cycle)
      apply/await -DEFAULT-> conclude

with the apply node scripted by outcomes (one per activation) and a counter of apply
activations: deterministic node bodies, real engine routing/pausing.
"""

from __future__ import annotations

from collections.abc import AsyncGenerator
from typing import Any

from google.adk.agents import Context
from google.adk.events import Event, EventActions
from google.adk.events.request_input import RequestInput
from google.adk.workflow import DEFAULT_ROUTE, START, Edge, FunctionNode, Workflow

from automation_agent.agent.setup.longrun import (
    NODE_OUTPUT_KEY,
    DriveResult,
    LongRunDriver,
)


class _Script:
    """Scripted apply outcomes (one per activation) plus an activation counter."""

    def __init__(self, outcomes: list[tuple[dict[str, Any], str]]) -> None:
        self.outcomes = outcomes
        self.calls = 0

    def next(self) -> tuple[dict[str, Any], str]:
        i = self.calls
        self.calls += 1
        if i >= len(self.outcomes):
            raise AssertionError(f"apply activation {i} exceeds scripted outcomes")
        return self.outcomes[i]


def _apply_ok() -> tuple[dict[str, Any], str]:
    return {NODE_OUTPUT_KEY: "apply", "pr_number": 7, "head_sha": "abc"}, "go_wait"


def _lr_graph(script: _Script) -> Workflow:
    async def apply(ctx: Context) -> AsyncGenerator[Any, None]:
        out, route = script.next()
        yield Event(output=out, actions=EventActions(route=route))

    async def await_ci(ctx: Context) -> AsyncGenerator[Any, None]:
        interrupt_id = f"await-{ctx.invocation_id}"
        reply = ctx.resume_inputs.get(interrupt_id)
        if reply is None:
            yield RequestInput(interrupt_id=interrupt_id, message="waiting", response_schema=None)
            return
        out: dict[str, Any] = {NODE_OUTPUT_KEY: "await"}
        route = "conclude"
        if isinstance(reply, dict):
            out.update(reply)
            if str(reply.get("conclusion")) == "failure":
                route = "failure"
        yield Event(output=out, actions=EventActions(route=route))

    def conclude() -> str:
        return "done"

    apply_node = FunctionNode(func=apply, name="apply")
    await_node = FunctionNode(func=await_ci, name="await_node", rerun_on_resume=True)
    conclude_node = FunctionNode(func=conclude, name="conclude")
    return Workflow(
        name="lr",
        edges=[
            Edge(from_node=START, to_node=apply_node),
            Edge(from_node=apply_node, to_node=await_node, route="go_wait"),
            Edge(from_node=apply_node, to_node=conclude_node, route=DEFAULT_ROUTE),
            Edge(from_node=await_node, to_node=apply_node, route="failure"),
            Edge(from_node=await_node, to_node=conclude_node, route=DEFAULT_ROUTE),
        ],
    )


def _new_driver(script: _Script) -> LongRunDriver:
    return LongRunDriver("lr-app", "u", _lr_graph(script))


async def test_long_run_driver_loop() -> None:
    """Start -> park -> resume(failure) -> re-park -> resume(success): apply runs once per
    attempt and the loop concludes."""
    script = _Script([_apply_ok(), _apply_ok()])
    d = _new_driver(script)

    start = await d.start("s1", "go")
    assert start.parked_call_id, "Start did not park on await"
    apply_out = start.node_output("apply")
    assert apply_out is not None and str(apply_out["pr_number"]) == "7"
    assert script.calls == 1

    # CI failed -> resume should re-apply and re-park.
    retry = await d.resume("s1", start.parked_call_id, {"conclusion": "failure"})
    assert retry.parked_call_id, "failure resume did not re-park"
    assert script.calls == 2

    # CI passed -> resume should conclude without re-parking.
    done = await d.resume("s1", retry.parked_call_id, {"conclusion": "success"})
    assert not done.parked_call_id, "success resume should not re-park"
    assert script.calls == 2, "apply must not run again on success"
    await_out = done.node_output("await")
    assert await_out is not None and str(await_out["conclusion"]) == "success"


async def test_long_run_driver_clean_stop() -> None:
    """A terminal apply result (routes to conclude, never to the wait node) finishes the
    run without parking, and its output is readable by tag."""
    script = _Script([({NODE_OUTPUT_KEY: "apply", "clean": True}, "conclude")])
    d = _new_driver(script)

    res = await d.start("s1", "go")
    assert not res.parked_call_id, "a clean (terminal) apply must not park"
    apply_out = res.node_output("apply")
    assert apply_out is not None and apply_out.get("clean") is True
    assert script.calls == 1


async def test_long_run_driver_apply_error() -> None:
    """An apply failure surfaces as an "error" output and concludes the run without
    parking it — the caller (not the engine) owns notifying a human, so the error must
    come back as data, not a failed run."""
    script = _Script([({NODE_OUTPUT_KEY: "apply", "error": "apply boom"}, "conclude")])
    d = _new_driver(script)

    res = await d.start("s1", "go")
    assert not res.parked_call_id, "a failed apply must not park"
    apply_out = res.node_output("apply")
    assert apply_out is not None and "error" in apply_out


async def test_long_run_driver_delete_session() -> None:
    """Terminal cleanup actually removes the stored session, so a durable backend does
    not leak completed runs; deleting a missing session is a no-op."""
    script = _Script([_apply_ok()])
    d = _new_driver(script)

    await d.start("s1", "go")
    existing = await d._session_service.get_session(app_name="lr-app", user_id="u", session_id="s1")
    assert existing is not None, "session should exist after start"
    await d.delete_session("s1")
    gone = await d._session_service.get_session(app_name="lr-app", user_id="u", session_id="s1")
    assert gone is None, "session should be gone after delete_session"
    await d.delete_session("s1")  # deleting a missing session must no-op


async def test_late_webhook_after_timeout() -> None:
    """A late/duplicate resume on a concluded run must not re-park (defense in depth
    behind the park store's atomic claim): the engine recognizes the interrupt as already
    resolved and no-ops the turn."""
    script = _Script([_apply_ok()])
    d = _new_driver(script)
    start = await d.start("s1", "go")

    # timeout concludes the run (route != failure -> conclude).
    timed_out = await d.resume("s1", start.parked_call_id, {"conclusion": "timeout"})
    assert not timed_out.parked_call_id

    # late webhook replays the same (now stale) interrupt id -> must not re-park.
    late = await d.resume("s1", start.parked_call_id, {"conclusion": "success"})
    assert not late.parked_call_id, "late webhook re-parked the run — would leak a parked run"


def test_drive_result_node_output() -> None:
    """Tag-based selection: latest per tag wins, and an unknown tag yields None."""
    res = DriveResult(
        node_outputs=[
            {NODE_OUTPUT_KEY: "apply", "attempt": 1},
            {NODE_OUTPUT_KEY: "await", "conclusion": "failure"},
            {NODE_OUTPUT_KEY: "apply", "attempt": 2},
        ]
    )
    apply_out = res.node_output("apply")
    assert apply_out is not None and apply_out["attempt"] == 2
    await_out = res.node_output("await")
    assert await_out is not None and await_out["conclusion"] == "failure"
    assert res.node_output("nope") is None
