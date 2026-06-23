"""Tests for setup long-run (sequencer + LongRunDriver).

Python ADK propagates a raised tool exception instead of converting it to an
``{"error": msg}`` response, so the ``apply`` tool here
returns ``{"error": ...}`` itself to exercise the sequencer's error branch — the same
convention fixflow's real ``apply_fix`` tool uses.
"""

from __future__ import annotations

from google.adk.agents import LlmAgent
from google.adk.tools import FunctionTool, LongRunningFunctionTool
from google.genai import types

from automation_agent.agent.setup.longrun import LongRunDriver, Sequencer


class _Tools:
    def __init__(self) -> None:
        self.calls = 0
        self.fail = False

    def apply(self) -> dict:
        self.calls += 1
        if self.fail:
            return {"error": "apply boom"}
        return {"pr_number": 7, "head_sha": "abc"}

    def await_ci(self, pr_number: int, head_sha: str) -> dict:
        return {"status": "pending"}


def _new_driver(tools: _Tools) -> LongRunDriver:
    seq = Sequencer(
        action="apply",
        wait="await_ci",
        retry_when=lambda r: str(r.get("conclusion")) == "failure",
    )
    agent = LlmAgent(
        name="lr",
        model=seq,
        instruction="apply then await",
        tools=[FunctionTool(tools.apply), LongRunningFunctionTool(tools.await_ci)],
    )
    return LongRunDriver("lr-app", "u", agent)


async def test_long_run_driver_loop() -> None:
    tools = _Tools()
    d = _new_driver(tools)

    start = await d.start("s1", "go")
    assert start.parked_call_id, "Start did not park on await_ci"
    assert str(start.tool_responses["apply"]["pr_number"]) == "7"
    assert tools.calls == 1

    # CI failed -> resume should re-apply and re-park.
    retry = await d.resume("s1", start.parked_call_id, "await_ci", {"conclusion": "failure"})
    assert retry.parked_call_id, "failure resume did not re-park"
    assert retry.parked_call_id != start.parked_call_id, "re-park should use a fresh call id"
    assert tools.calls == 2

    # CI passed -> resume should conclude without re-parking.
    done = await d.resume("s1", retry.parked_call_id, "await_ci", {"conclusion": "success"})
    assert not done.parked_call_id, "success resume should not re-park"
    assert tools.calls == 2, "apply must not run again on success"
    assert "done" in done.final


async def test_long_run_driver_apply_error() -> None:
    tools = _Tools()
    tools.fail = True
    d = _new_driver(tools)

    res = await d.start("s1", "go")
    assert not res.parked_call_id, "a failed apply must not park"
    assert "error" in res.tool_responses["apply"]
    assert "failed" in res.final


def test_sequencer_decide() -> None:
    s = Sequencer(
        action="apply",
        wait="await_ci",
        retry_when=lambda r: str(r.get("conclusion")) == "failure",
    )

    def fc_name(content: types.Content) -> tuple[str, str]:
        text = ""
        for p in content.parts or []:
            if p.function_call is not None:
                return p.function_call.name, ""
            if p.text:
                text += p.text
        return "", text

    def resp(name: str, body: dict) -> types.Content:
        return types.Content(
            parts=[types.Part(function_response=types.FunctionResponse(name=name, response=body))]
        )

    # No history -> call apply.
    assert fc_name(s._decide([]).content)[0] == "apply"
    # apply ok -> call await_ci.
    assert fc_name(s._decide([resp("apply", {"pr_number": 7})]).content)[0] == "await_ci"
    # apply error -> conclude.
    name, text = fc_name(s._decide([resp("apply", {"error": "x"})]).content)
    assert name == "" and "failed" in text
    # await_ci failure -> retry apply.
    assert fc_name(s._decide([resp("await_ci", {"conclusion": "failure"})]).content)[0] == "apply"
    # await_ci success -> conclude.
    name, text = fc_name(s._decide([resp("await_ci", {"conclusion": "success"})]).content)
    assert name == "" and text != ""


async def test_late_webhook_after_timeout() -> None:
    """A late/duplicate resume on a concluded run must not re-park (defense in depth
    behind the park store's atomic claim)."""
    tools = _Tools()
    d = _new_driver(tools)
    start = await d.start("s1", "go")

    # timeout concludes the run (retry_when false for "timeout").
    timed_out = await d.resume("s1", start.parked_call_id, "await_ci", {"conclusion": "timeout"})
    assert not timed_out.parked_call_id

    # late webhook replays the same (now stale) call id -> must not re-park.
    late = await d.resume("s1", start.parked_call_id, "await_ci", {"conclusion": "success"})
    assert not late.parked_call_id, "late webhook re-parked the run — would leak a parked run"
