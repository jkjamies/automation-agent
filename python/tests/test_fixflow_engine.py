"""Port of fixflow engine_test.go: the full kickoff -> park -> resume loop driven
through the REAL setup.LongRunDriver, with fake (non-LLM) triage/analyze, a seed remote,
a fake GitHub, and a fake Notifier.
"""

from __future__ import annotations

from datetime import timedelta

import pytest
from git import Actor
from git import Repo as GitRepo

from automation_agent.agent.fixflow import (
    AnalyzeInput,
    Deps,
    Engine,
    FileEdit,
    FileWork,
    ParkedRun,
    Spec,
    new_engine,
)
from automation_agent.githubapi import PR, PRInput
from automation_agent.notify import Message

# --- fakes ------------------------------------------------------------------


class FakeGH:
    def __init__(self, existing: list[PR] | None = None) -> None:
        self.existing = existing or []
        self.created: PRInput | None = None
        self.labeled: list[str] = []

    def find_agent_prs(self, owner: str, repo: str, label: str) -> list[PR]:
        return self.existing

    def create_pr(self, owner: str, repo: str, in_: PRInput) -> PR:
        self.created = in_
        return PR(number=42, title=in_.title, branch=in_.head, head_sha="", url="https://gh/pr/42")

    def add_labels(self, owner: str, repo: str, number: int, *labels: str) -> None:
        self.labeled.extend(labels)


class FakeNotifier:
    def __init__(self) -> None:
        self.msgs: list[Message] = []

    def notify(self, m: Message) -> None:
        self.msgs.append(m)


def _seed_remote(tmp_path, name: str = "remote") -> str:
    dir_ = tmp_path / name
    dir_.mkdir()
    repo = GitRepo.init(dir_)
    (dir_ / "README.md").write_text("hi")
    repo.index.add(["README.md"])
    repo.index.commit("init", author=Actor("seed", "s@x"), committer=Actor("seed", "s@x"))
    return str(dir_)


async def _triage(_llm, _report) -> list[FileWork]:
    return [FileWork(path="a.go", items=["x"])]


async def _analyze(_in: AnalyzeInput) -> list[FileEdit]:
    return [FileEdit(path="a.go", content="package a\n")]


def _spec() -> Spec:
    return Spec(
        name="test", branch="agent/fix", label="automation-agent",
        check_name="agent-test-verify", commit_message="fix", pr_title="Fix",
        success_title="Fix succeeded", review_title="Needs human review",
        triage=_triage, analyze=_analyze,
    )


def _new_engine(remote: str, gh: FakeGH, n: FakeNotifier) -> Engine:
    return new_engine(
        _spec(),
        Deps(gh=gh, notify=n, max_iter=3, ci_timeout=timedelta(hours=1),
             clone_url=lambda _o, _r: remote),
    )


def _check_body(conclusion: str, pr: int, output: str = "") -> bytes:
    import json

    payload = {
        "action": "completed",
        "check_run": {
            "name": "agent-test-verify", "status": "completed", "conclusion": conclusion,
            "pull_requests": [{"number": pr, "head": {"ref": "agent/fix"}}],
            "output": {"text": output},
        },
        "repository": {"full_name": "acme/api"},
    }
    return json.dumps(payload).encode()


# --- tests ------------------------------------------------------------------


async def test_engine_kickoff_parks(tmp_path) -> None:
    remote = _seed_remote(tmp_path)
    gh = FakeGH()
    e = _new_engine(remote, gh, FakeNotifier())

    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')
    assert gh.created is not None and gh.created.head == "agent/fix"
    assert len(gh.labeled) == 1
    assert e.driver.reg.len() == 1


async def test_engine_resume_success(tmp_path) -> None:
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')
    await e.resume(_check_body("success", 42))
    assert len(n.msgs) == 1 and "succeeded" in n.msgs[0].title
    assert e.driver.reg.len() == 0


async def test_engine_resume_exhausted(tmp_path) -> None:
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    e.driver.reg.park(
        "acme/api#42", ParkedRun(session_id="run-x", call_id="c", attempts=3),
        timedelta(hours=1), e.driver.on_timeout,
    )
    await e.resume(_check_body("failure", 42, "still broken"))
    assert len(n.msgs) == 1 and "review" in n.msgs[0].title
    assert e.driver.reg.len() == 0


async def test_engine_resume_retry(tmp_path) -> None:
    remote = _seed_remote(tmp_path)
    gh = FakeGH()
    n = FakeNotifier()
    e = _new_engine(remote, gh, n)

    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')

    async def retry_analyze(_in: AnalyzeInput) -> list[FileEdit]:
        return [FileEdit(path="a.go", content="package a\n\n// retry\n")]

    e.spec.analyze = retry_analyze
    gh.existing = [PR(number=42, title="", branch="agent/fix", head_sha="", url="")]
    gh.created = None

    await e.resume(_check_body("failure", 42, "still failing"))
    assert gh.created is None  # reused, not created
    assert len(n.msgs) == 0
    assert e.driver.reg.len() == 1


async def test_engine_full_loop_exhausts(tmp_path) -> None:
    remote = _seed_remote(tmp_path)
    gh = FakeGH(existing=[PR(number=42, title="", branch="agent/fix", head_sha="", url="")])
    n = FakeNotifier()
    spec = _spec()
    calls = {"n": 0}

    async def varying(_in: AnalyzeInput) -> list[FileEdit]:
        calls["n"] += 1
        return [FileEdit(path="a.go", content=f"package a\n// v{calls['n']}\n")]

    spec.analyze = varying
    e = new_engine(
        spec,
        Deps(gh=gh, notify=n, max_iter=3, ci_timeout=timedelta(hours=1),
             clone_url=lambda _o, _r: remote),
    )

    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')
    # Two failures are retried (attempts 2, 3); the third gives up.
    for _ in range(2):
        await e.resume(_check_body("failure", 42, "boom"))
        assert len(n.msgs) == 0
        assert e.driver.reg.len() == 1
    await e.resume(_check_body("failure", 42, "boom"))
    assert len(n.msgs) == 1 and "review" in n.msgs[0].title
    assert e.driver.reg.len() == 0
    assert calls["n"] == 3


async def test_engine_timeout_frees_run(tmp_path) -> None:
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    e.driver.reg.park(
        "acme/api#42", ParkedRun(session_id="run-x", call_id="c", attempts=1),
        timedelta(hours=1), e.driver.on_timeout,
    )
    await e.driver.on_timeout("acme/api#42")
    assert len(n.msgs) == 1 and "review" in n.msgs[0].title
    assert e.driver.reg.len() == 0
    # A late webhook after the timeout is a benign no-op.
    await e.resume(_check_body("success", 42))
    assert len(n.msgs) == 1


async def test_engine_resume_unknown_pr(tmp_path) -> None:
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    await e.resume(_check_body("success", 99))
    assert len(n.msgs) == 0


async def test_engine_resume_ignores_other_check(tmp_path) -> None:
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    body = b'{"check_run":{"name":"some-other-check","status":"completed","conclusion":"failure"},"repository":{"full_name":"acme/api"}}'
    await e.resume(body)
    assert len(n.msgs) == 0


async def test_engine_kickoff_triage_error(tmp_path) -> None:
    spec = _spec()

    async def boom(_llm, _report) -> list[FileWork]:
        raise RuntimeError("triage boom")

    spec.triage = boom
    e = new_engine(
        spec,
        Deps(gh=FakeGH(), ci_timeout=timedelta(hours=1),
             clone_url=lambda _o, _r: _seed_remote(tmp_path, "r2")),
    )
    with pytest.raises(RuntimeError):
        await e.kickoff(b'{"repo":"acme/api","report":"r"}')
    assert e.driver.reg.len() == 0


def test_engine_label_and_check_name(tmp_path) -> None:
    e = _new_engine("x", FakeGH(), FakeNotifier())
    assert e.label() == "automation-agent"
    assert e.check_name() == "agent-test-verify"
