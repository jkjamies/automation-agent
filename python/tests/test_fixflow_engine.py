"""Tests for the fixflow engine: the full kickoff -> park -> resume loop driven
through the REAL setup.LongRunDriver, with fake (non-LLM) triage/analyze, a seed remote,
a fake GitHub, and a fake Notifier.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

import pytest
from git import Actor
from git import Repo as GitRepo

from automation_agent.agent.fixflow import (
    AnalyzeInput,
    Deps,
    Engine,
    FileEdit,
    FileWork,
    NoWorkError,
    Spec,
    new_engine,
)
from automation_agent.agent.fixflow.driver import RunParams
from automation_agent.agent.setup import ParkRecord
from automation_agent.githubapi import PR, ChangedFile, Comparison, PRInput
from automation_agent.notify import Message


async def _prepark(
    e: Engine,
    key: str,
    *,
    session_id: str,
    attempts: int,
    parked_at: datetime | None = None,
) -> None:
    """Seed a parked run directly in the store (bypassing kickoff) so the exhausted /
    timeout / sweep terminal paths can be exercised in isolation. Stores valid run params so
    the terminal summary can decode the findings."""
    repo, _, _num = key.partition("#")
    owner, _, name = repo.partition("/")
    params = RunParams(
        owner=owner, repo=name, full_repo=repo, base="main", report="lint findings"
    ).to_json()
    await e.driver.store.put(
        ParkRecord(
            session_id=session_id,
            pr_key=key,
            call_id="c",
            attempts=attempts,
            params=params,
            parked_at=parked_at or datetime.now(UTC),
        )
    )


# --- fakes ------------------------------------------------------------------


class FakeGH:
    def __init__(self, existing: list[PR] | None = None) -> None:
        self.existing = existing or []
        self.created: PRInput | None = None
        self.labeled: list[str] = []

    def find_open_pr_by_branch(self, owner: str, repo: str, branch: str) -> PR | None:
        return next((pr for pr in self.existing if pr.branch == branch), None)

    def create_pr(self, owner: str, repo: str, in_: PRInput) -> PR:
        self.created = in_
        return PR(number=42, title=in_.title, branch=in_.head, head_sha="", url="https://gh/pr/42")

    def add_labels(self, owner: str, repo: str, number: int, *labels: str) -> None:
        self.labeled.extend(labels)

    def compare(self, owner: str, repo: str, base: str, head: str) -> Comparison:
        return Comparison(
            total_commits=1,
            files=[ChangedFile(path="a.go", status="modified", additions=3, deletions=1)],
        )


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
        name="test",
        branch="agent/fix",
        check_name="agent-test-verify",
        commit_message="fix",
        pr_title="Fix",
        success_title="Fix succeeded",
        review_title="Needs human review",
        clean_title="Already clean",
        triage=_triage,
        analyze=_analyze,
    )


def _new_engine(remote: str, gh: FakeGH, n: FakeNotifier) -> Engine:
    return new_engine(
        _spec(),
        Deps(
            gh=gh,
            notify=n,
            pr_label="automation-agent",
            max_iter=3,
            ci_timeout=timedelta(hours=1),
            clone_url=lambda _o, _r: remote,
        ),
    )


def _check_body(conclusion: str, pr: int, output: str = "") -> bytes:
    import json

    payload = {
        "action": "completed",
        "check_run": {
            "name": "agent-test-verify",
            "status": "completed",
            "conclusion": conclusion,
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
    assert await e.driver.parked_count() == 1


async def test_engine_kickoff_rejects_repo_not_in_allowlist() -> None:
    gh = FakeGH()
    e = new_engine(
        _spec(),
        Deps(
            gh=gh, notify=FakeNotifier(), repos=["allowed/repo"], clone_url=lambda _o, _r: "unused"
        ),
    )
    with pytest.raises(ValueError, match="allowlist"):
        await e.kickoff(b'{"repo":"acme/api","report":"r"}')
    assert gh.created is None
    assert await e.driver.parked_count() == 0


async def test_engine_kickoff_allows_repo_in_allowlist(tmp_path) -> None:
    remote = _seed_remote(tmp_path)
    gh = FakeGH()
    e = new_engine(
        _spec(),
        Deps(
            gh=gh,
            notify=FakeNotifier(),
            repos=["acme/api"],
            ci_timeout=timedelta(hours=1),
            clone_url=lambda _o, _r: remote,
        ),
    )
    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')
    assert gh.created is not None and gh.created.head == "agent/fix"
    assert await e.driver.parked_count() == 1


async def test_engine_resume_success(tmp_path) -> None:
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')
    await e.resume(_check_body("success", 42))
    assert len(n.msgs) == 1 and "succeeded" in n.msgs[0].title
    # The status-aware summary reports the outcome + what changed on the PR (from compare).
    body = n.msgs[0].text
    assert "passed CI" in body and "a.go" in body
    assert await e.driver.parked_count() == 0


async def test_engine_resume_exhausted(tmp_path) -> None:
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    await _prepark(e, "acme/api#42", session_id="run-x", attempts=3)
    await e.resume(_check_body("failure", 42, "still broken"))
    assert len(n.msgs) == 1 and "review" in n.msgs[0].title
    assert await e.driver.parked_count() == 0


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
    assert await e.driver.parked_count() == 1


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
        Deps(
            gh=gh,
            notify=n,
            pr_label="automation-agent",
            max_iter=3,
            ci_timeout=timedelta(hours=1),
            clone_url=lambda _o, _r: remote,
        ),
    )

    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')
    # Two failures are retried (attempts 2, 3); the third gives up.
    for _ in range(2):
        await e.resume(_check_body("failure", 42, "boom"))
        assert len(n.msgs) == 0
        assert await e.driver.parked_count() == 1
    await e.resume(_check_body("failure", 42, "boom"))
    assert len(n.msgs) == 1 and "review" in n.msgs[0].title
    assert await e.driver.parked_count() == 0
    assert calls["n"] == 3


async def test_engine_timeout_frees_run(tmp_path) -> None:
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    await _prepark(e, "acme/api#42", session_id="run-x", attempts=1)
    await e.driver.on_timeout("acme/api#42")
    assert len(n.msgs) == 1 and "review" in n.msgs[0].title
    assert await e.driver.parked_count() == 0
    # A late webhook after the timeout is a benign no-op.
    await e.resume(_check_body("success", 42))
    assert len(n.msgs) == 1


async def test_engine_sweep_times_out_stale_runs(tmp_path) -> None:
    # The durable catch-all (driven by /internal/sweep): frees runs whose CI never
    # reported, leaving fresher ones parked. (ci_timeout is 1h via _new_engine.)
    n = FakeNotifier()
    e = _new_engine(_seed_remote(tmp_path), FakeGH(), n)
    stale = datetime.now(UTC) - timedelta(hours=2)
    await _prepark(e, "acme/api#42", session_id="run-stale", attempts=1, parked_at=stale)
    await _prepark(e, "acme/api#43", session_id="run-fresh", attempts=1)  # parked_at=now

    await e.sweep_timeouts()
    assert len(n.msgs) == 1 and "review" in n.msgs[0].title
    assert await e.driver.parked_count() == 1  # only the stale run was swept
    # A late webhook for the swept PR is a benign no-op; the fresh run still resolves.
    await e.resume(_check_body("success", 42))
    assert len(n.msgs) == 1
    await e.resume(_check_body("success", 43))
    assert await e.driver.parked_count() == 0


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
        Deps(
            gh=FakeGH(),
            ci_timeout=timedelta(hours=1),
            clone_url=lambda _o, _r: _seed_remote(tmp_path, "r2"),
        ),
    )
    with pytest.raises(RuntimeError):
        await e.kickoff(b'{"repo":"acme/api","report":"r"}')
    assert await e.driver.parked_count() == 0


async def test_engine_kickoff_clean(tmp_path) -> None:
    # Triage finding nothing actionable finishes as a positive clean outcome: no PR is
    # opened, no run is parked, the clean notification (not the review alarm) is sent, and
    # kickoff does not raise so the dispatcher does not log a no-op as a failure.
    spec = _spec()

    async def nothing(_llm, _report) -> list[FileWork]:
        raise NoWorkError("triage: nothing here")

    spec.triage = nothing
    gh = FakeGH()
    n = FakeNotifier()
    e = new_engine(
        spec,
        Deps(
            gh=gh,
            notify=n,
            ci_timeout=timedelta(hours=1),
            clone_url=lambda _o, _r: _seed_remote(tmp_path, "r3"),
        ),
    )

    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')
    assert gh.created is None
    assert await e.driver.parked_count() == 0
    assert len(n.msgs) == 1
    assert n.msgs[0].title == "Already clean"
    assert "review" not in n.msgs[0].text.lower()


async def test_engine_kickoff_apply_failure_notifies(tmp_path) -> None:
    # An apply-step failure (here: PR creation) must ask a human to review rather than
    # vanishing silently. (C2)
    remote = _seed_remote(tmp_path)
    gh = FakeGH()

    def boom_create_pr(owner, repo, in_):  # type: ignore[no-untyped-def]
        raise RuntimeError("create PR exploded")

    gh.create_pr = boom_create_pr  # type: ignore[assignment]
    n = FakeNotifier()
    e = _new_engine(remote, gh, n)

    with pytest.raises(RuntimeError):
        await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')
    assert len(n.msgs) == 1 and "review" in n.msgs[0].title.lower()
    assert await e.driver.parked_count() == 0


async def test_engine_triage_runs_each_attempt(tmp_path) -> None:
    # Triage re-runs on every attempt (no in-process cache): a retry resumes on a fresh
    # process, so a cache would miss anyway — matches Go/Ko/JS. (S6)
    remote = _seed_remote(tmp_path)
    gh = FakeGH()
    n = FakeNotifier()
    spec = _spec()
    triage_calls = {"n": 0}

    async def counting_triage(_llm, _report) -> list[FileWork]:
        triage_calls["n"] += 1
        return [FileWork(path="a.go", items=["x"])]

    spec.triage = counting_triage
    e = new_engine(
        spec,
        Deps(
            gh=gh,
            notify=n,
            pr_label="automation-agent",
            max_iter=3,
            ci_timeout=timedelta(hours=1),
            clone_url=lambda _o, _r: remote,
        ),
    )

    await e.kickoff(b'{"repo":"acme/api","base":"master","report":"r"}')

    async def retry_analyze(_in: AnalyzeInput) -> list[FileEdit]:
        return [FileEdit(path="a.go", content="package a\n\n// retry\n")]

    e.spec.analyze = retry_analyze
    gh.existing = [PR(number=42, title="", branch="agent/fix", head_sha="", url="")]
    await e.resume(_check_body("failure", 42, "still failing"))
    assert triage_calls["n"] == 2  # kickoff + retry each re-triage
    assert await e.driver.parked_count() == 1


def test_engine_label_and_check_name(tmp_path) -> None:
    e = _new_engine("x", FakeGH(), FakeNotifier())
    assert e.label() == "automation-agent"
    assert e.check_name() == "agent-test-verify"


def test_clone_url_transport() -> None:
    # Default (https) and an explicit ssh transport build the two GitHub URL forms; a
    # test-injected clone_url overrides both.
    https = new_engine(_spec(), Deps(gh=FakeGH()))
    assert https._clone_url("acme", "api") == "https://github.com/acme/api.git"

    ssh = new_engine(_spec(), Deps(gh=FakeGH(), git_transport="ssh"))
    assert ssh._clone_url("acme", "api") == "git@github.com:acme/api.git"

    override = new_engine(
        _spec(), Deps(gh=FakeGH(), git_transport="ssh", clone_url=lambda _o, _r: "x")
    )
    assert override._clone_url("acme", "api") == "x"
