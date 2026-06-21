"""Port of fixflow units_test.go + tools_test.go: pure helpers, path-safety, envelope,
and the repo tools.
"""

from __future__ import annotations

import os

import pytest

from automation_agent.agent.fixflow import (
    extract_json_array,
    extract_json_object,
    parse_kickoff,
    read_file,
    repo_tools,
    strip_fences,
)
from automation_agent.agent.fixflow.analyze import parallel_analyze
from automation_agent.agent.fixflow.applyfix import FileEdit
from automation_agent.agent.fixflow.engine import FileWork
from automation_agent.agent.fixflow.files import _safe_join
from automation_agent.agent.fixflow.tools import list_dir_entries

# --- envelope ---------------------------------------------------------------


def test_parse_kickoff() -> None:
    k = parse_kickoff(b'{"repo":"acme/api","report":{"x":1}}')
    assert k.owner() == "acme"
    assert k.name() == "api"
    assert k.base == "main"
    assert k.report_text() != ""

    for body in (b"{", b'{"report":{"x":1}}', b'{"repo":"noslash","report":{"x":1}}', b'{"repo":"a/b"}'):
        with pytest.raises(ValueError):
            parse_kickoff(body)


def test_report_text() -> None:
    k = parse_kickoff(b'{"repo":"a/b","report":{"x":1}}')
    assert k.report_text() == '{"x":1}'
    k2 = parse_kickoff(b'{"repo":"a/b","report":"TN:\\nSF:calc.go\\nDA:7,0\\n"}')
    assert k2.report_text() == "TN:\nSF:calc.go\nDA:7,0\n"


# --- util -------------------------------------------------------------------


def test_extract_and_strip() -> None:
    assert extract_json_array("noise [1,2] x") == "[1,2]"
    assert extract_json_array("none") == ""
    assert extract_json_object('x {"a":1} y') == '{"a":1}'
    assert extract_json_object("none") == ""
    assert strip_fences("```go\npackage x\n```") == "package x\n"
    assert strip_fences("package y") == "package y\n"


# --- files / safe_join ------------------------------------------------------


def test_read_file_and_safe_join(tmp_path) -> None:
    (tmp_path / "a.txt").write_text("hello")
    assert read_file(str(tmp_path), "a.txt") == "hello"
    with pytest.raises(ValueError):
        read_file(str(tmp_path), "../../etc/passwd")


def test_safe_join_rejects_escapes(tmp_path) -> None:
    root = str(tmp_path)
    for bad in ("../escape", "../../etc/cron.d/x", "/etc/passwd", "a/../../b"):
        with pytest.raises(ValueError):
            _safe_join(root, bad)
    for ok in ("a.go", "sub/dir/b_test.go", "."):
        _safe_join(root, ok)  # must not raise


# --- tools ------------------------------------------------------------------


def test_list_dir_entries(tmp_path) -> None:
    os.makedirs(tmp_path / "sub")
    os.makedirs(tmp_path / ".git")
    (tmp_path / "f.go").write_text("x")

    ents = list_dir_entries(str(tmp_path), ".")
    joined = ",".join(ents)
    assert "f.go" in joined
    assert "sub/" in joined
    assert ".git" not in joined
    with pytest.raises(ValueError):
        list_dir_entries(str(tmp_path), "../..")


def test_repo_tools(tmp_path) -> None:
    tools = repo_tools(str(tmp_path))
    assert len(tools) == 2


def test_repo_tools_read_and_list(tmp_path) -> None:
    (tmp_path / "AGENTS.md").write_text("docs")
    os.makedirs(tmp_path / "src")
    tools = repo_tools(str(tmp_path))
    names = {t.name for t in tools}
    assert names == {"read_file", "list_dir"}


# --- parallel_analyze -------------------------------------------------------


async def test_parallel_analyze() -> None:
    work = [FileWork(path="b.go"), FileWork(path="a.go")]

    async def fn(w: FileWork) -> FileEdit:
        return FileEdit(path=w.path + "_test.go", content="package x\n")

    edits = await parallel_analyze(work, fn)
    assert len(edits) == 2
    assert edits[0].path == "a.go_test.go"
    assert edits[1].path == "b.go_test.go"


async def test_parallel_analyze_skips() -> None:
    async def skip(_w: FileWork) -> FileEdit:
        return FileEdit(path="", content="")

    with pytest.raises(ValueError, match="no edits"):
        await parallel_analyze([FileWork(path="a.go")], skip)

    async def never(_w: FileWork) -> FileEdit:  # pragma: no cover - not called
        return FileEdit(path="", content="")

    with pytest.raises(ValueError):
        await parallel_analyze([], never)
