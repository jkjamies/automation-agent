"""Port of covfixer coverage_test.go: triage, explore-plan parse, and the two-phase
analyze (explore -> execute), plus engine identity.

The ``ScriptedLlm`` routes by system instruction (triage / explore-plan / execute),
mirroring Go's scriptedLLM. We assert on structure (paths, plan keys), never on
LLM-authored content.
"""

from __future__ import annotations

import pytest
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.genai import types
from pydantic import PrivateAttr

from automation_agent.agent.covfixer import new_engine, triage
from automation_agent.agent.covfixer.analyze import (
    PlanEntry,
    _build_execute_input,
    _parse_plan,
    analyze,
)
from automation_agent.agent.covfixer.triage import _parse_triage
from automation_agent.agent.fixflow import AnalyzeInput, Deps, FileWork
from automation_agent.agent.setup import content_text


class ScriptedLlm(BaseLlm):
    """Routes by the system instruction: triage, explore (plan), or execute (test)."""

    _triage: str = PrivateAttr(default="")
    _plan: str = PrivateAttr(default="")
    _test: str = PrivateAttr(default="")

    def __init__(self, triage: str = "", plan: str = "", test: str = "") -> None:
        super().__init__(model="scripted")
        self._triage, self._plan, self._test = triage, plan, test

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False):  # type: ignore[override]
        resp = self._test
        sys = ""
        if llm_request.config is not None:
            si = llm_request.config.system_instruction
            sys = si if isinstance(si, str) else content_text(si)
        if "triaging" in sys:
            resp = self._triage
        elif "planning where to add" in sys:
            resp = self._plan
        yield LlmResponse(
            content=types.Content(role="model", parts=[types.Part.from_text(text=resp)]),
            turn_complete=True,
        )


def test_parse_triage() -> None:
    work = _parse_triage(
        '[{"path":"calc.go","uncovered":["Divide error path","Add edge cases"]},'
        '{"path":"","uncovered":[]}]'
    )
    assert len(work) == 1
    assert work[0].path == "calc.go"
    assert len(work[0].items) == 2


async def test_triage() -> None:
    work = await triage(
        ScriptedLlm(triage='[{"path":"calc.go","uncovered":["Divide"]}]'), "jacoco xml"
    )
    assert len(work) == 1 and work[0].path == "calc.go"
    with pytest.raises(ValueError):
        await triage(ScriptedLlm(triage="[]"), "report")


def test_parse_plan() -> None:
    plan = _parse_plan(
        'prose [{"source":"calc.go","test_path":"calc_test.go","framework":"go testing",'
        '"notes":"package calc"},{"source":"","test_path":"x"}] more'
    )
    assert len(plan) == 1
    assert plan["calc.go"].test_path == "calc_test.go"
    assert plan["calc.go"].framework == "go testing"


async def test_analyze(tmp_path) -> None:
    (tmp_path / "calc.go").write_text(
        "package calc\nfunc Divide(a,b int)(int,error){return a/b,nil}"
    )
    (tmp_path / "existing_test.go").write_text(
        'package calc\nimport "testing"\nfunc TestExisting(t *testing.T){}'
    )
    llm = ScriptedLlm(
        plan='[{"source":"calc.go","test_path":"calc_test.go","framework":"go testing",'
        '"notes":"package calc"}]',
        test='package calc\n\nimport "testing"\n\nfunc TestDivide(t *testing.T) {}\n',
    )
    in_ = AnalyzeInput(
        llm=llm, code_llm=None, repo_dir=str(tmp_path),
        work=[FileWork(path="calc.go", items=["Divide"])],
    )
    edits = await analyze(in_)
    assert len(edits) == 1
    assert edits[0].path == "calc_test.go"
    assert "TestDivide" in edits[0].content


def test_build_execute_input() -> None:
    got = _build_execute_input(
        FileWork(path="calc.go", items=["Divide"]),
        "package calc",
        PlanEntry(source="calc.go", test_path="calc_test.go", framework="go testing", notes="pkg calc"),
        "ci failed",
    )
    for w in ("calc_test.go", "go testing", "pkg calc", "Divide", "package calc", "ci failed"):
        assert w in got


def test_new_engine() -> None:
    e = new_engine(Deps())
    assert e.check_name() == "agent-coverage-verify"
    assert e.label() == "automation-agent-coverage"
