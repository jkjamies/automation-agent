"""Port of lintfixer lint_test.go: triage parsing, analyze edits, and engine identity."""

from __future__ import annotations

import pytest

from automation_agent.agent.fixflow import AnalyzeInput, Deps, FileWork
from automation_agent.agent.lintfixer import new_engine, triage
from automation_agent.agent.lintfixer.analyze import _build_file_prompt, analyze
from automation_agent.agent.lintfixer.triage import _parse_triage


def test_parse_triage() -> None:
    work = _parse_triage(
        'x [{"path":"a.go","problems":["unchecked error"]},{"path":"","problems":[]}] y'
    )
    assert len(work) == 1
    assert work[0].path == "a.go"
    assert len(work[0].items) == 1


async def test_triage(fake_llm) -> None:
    work = await triage(fake_llm('[{"path":"a.go","problems":["x"]}]'), "report")
    assert len(work) == 1 and work[0].path == "a.go"
    with pytest.raises(ValueError):
        await triage(fake_llm("[]"), "report")


def test_build_file_prompt() -> None:
    p = _build_file_prompt(
        FileWork(path="a.go", items=["unchecked error"]), "package a", "ci failed"
    )
    for want in ("a.go", "unchecked error", "package a", "ci failed"):
        assert want in p


async def test_analyze(tmp_path, fake_llm) -> None:
    (tmp_path / "a.go").write_text("package a")
    in_ = AnalyzeInput(
        llm=fake_llm("package fixed\n"),
        code_llm=None,
        repo_dir=str(tmp_path),
        work=[FileWork(path="a.go", items=["x"])],
    )
    edits = await analyze(in_)
    assert len(edits) == 1
    assert edits[0].path == "a.go"
    assert edits[0].content == "package fixed\n"


def test_new_engine() -> None:
    e = new_engine(Deps())
    assert e.check_name() == "agent-lint-verify"
    assert e.label() == "automation-agent"
