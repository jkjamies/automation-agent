"""Tests for the summary workflow."""

from __future__ import annotations

from datetime import datetime

import pytest
from google.adk.agents import LlmAgent, ParallelAgent, SequentialAgent

from automation_agent.agent import setup
from automation_agent.agent.summary import (
    Deps,
    build_instruction,
    build_summary_agent,
    format_commits,
    safe_name,
    split_repo,
    summary_instruction,
)
from automation_agent.githubapi import Commit
from automation_agent.notify import Message

# --- fakes ---


class FakeLister:
    """A CommitLister returning canned commits keyed by owner/repo."""

    def __init__(
        self,
        by_repo: dict[str, list[Commit]] | None = None,
        error: Exception | None = None,
    ) -> None:
        self._by_repo = by_repo or {}
        self._error = error

    def list_commits_since(
        self, owner: str, repo: str, since: datetime
    ) -> list[Commit]:
        if self._error is not None:
            raise self._error
        return self._by_repo.get(f"{owner}/{repo}", [])


class FakeNotifier:
    """A Notifier capturing the Messages it is asked to post."""

    def __init__(self) -> None:
        self.msgs: list[Message] = []

    def notify(self, m: Message) -> None:
        self.msgs.append(m)


def _commit(sha: str, message: str, author: str) -> Commit:
    return Commit(sha=sha, message=message, author=author, url="", when=None)


# --- deterministic helpers ---


def test_format_commits_empty() -> None:
    got = format_commits("o/r", [])
    assert "no commits" in got


def test_format_commits_exact() -> None:
    commits = [_commit("abcdef1234", "fix bug\n\ndetails", "Jane")]
    got = format_commits("o/r", commits)
    assert got == "Repository o/r (1 commits):\n- abcdef1 fix bug (Jane)\n"
    # First line only — body should not leak.
    assert "details" not in got


def test_build_instruction_sorted_and_filtered() -> None:
    state = {
        "commits:b/b": "repo B data",
        "commits:a/a": "repo A data",
        "other": "ignore me",
    }
    got = build_instruction("PROMPT", state)
    assert got.startswith("PROMPT")
    assert "ignore me" not in got
    ai, bi = got.index("repo A data"), got.index("repo B data")
    assert 0 <= ai < bi


def test_build_instruction_empty() -> None:
    assert "no commit data" in build_instruction("P", {})


def test_summary_instruction_provider_reads_state() -> None:
    provider = summary_instruction("PROMPT")

    class _Ctx:
        state = {"commits:o/r": "the commit data"}

    got = provider(_Ctx())  # type: ignore[arg-type]
    assert got.startswith("PROMPT")
    assert "the commit data" in got


def test_split_repo_and_safe_name() -> None:
    assert split_repo("owner/repo") == ("owner", "repo")
    assert split_repo("bad") is None
    assert safe_name("a/b:c") == "a_b_c"


# --- structure ---


def test_build_summary_agent_structure(fake_llm) -> None:
    a = build_summary_agent(
        Deps(
            llm=fake_llm("digest"),
            gh=FakeLister(),
            notify=FakeNotifier(),
            repos=["o/r", "a/b"],
        )
    )
    assert isinstance(a, SequentialAgent)
    assert a.name == "summary_workflow"
    assert len(a.sub_agents) == 3
    parallel, summarizer, notifier = a.sub_agents
    assert isinstance(parallel, ParallelAgent)
    assert parallel.name == "fetch_all"
    # One fetcher per repo.
    assert len(parallel.sub_agents) == 2
    assert [s.name for s in parallel.sub_agents] == ["fetch_o_r", "fetch_a_b"]
    assert isinstance(summarizer, LlmAgent)
    assert summarizer.name == "summarizer"
    assert summarizer.output_key == "digest"
    assert notifier.name == "notify"


def test_build_summary_agent_validation(fake_llm) -> None:
    with pytest.raises(ValueError, match="at least one repo"):
        build_summary_agent(
            Deps(llm=fake_llm(""), gh=FakeLister(), notify=FakeNotifier(), repos=[])
        )
    with pytest.raises(ValueError, match="required"):
        build_summary_agent(
            Deps(llm=None, gh=None, notify=None, repos=["o/r"])  # type: ignore[arg-type]
        )


# --- behavior through a real runner ---


async def test_fetch_agent_writes_state_key(fake_llm) -> None:
    gh = FakeLister(
        by_repo={"o/r": [_commit("abc1234", "do the thing", "X")]}
    )
    a = build_summary_agent(
        Deps(llm=fake_llm("digest"), gh=gh, notify=FakeNotifier(), repos=["o/r"])
    )
    runner = setup.new_runner("summary-test", a)
    state = await setup.drive_collect_state(runner, "u", "s", "go")
    assert "commits:o/r" in state
    assert "do the thing" in state["commits:o/r"]


async def test_notifier_posts_digest(fake_llm) -> None:
    gh = FakeLister(
        by_repo={"o/r": [_commit("abc1234", "do the thing", "X")]}
    )
    notifier = FakeNotifier()
    a = build_summary_agent(
        Deps(llm=fake_llm("THE DIGEST"), gh=gh, notify=notifier, repos=["o/r"])
    )
    runner = setup.new_runner("summary-test", a)
    await setup.drive(runner, "u", "s", "go")
    assert len(notifier.msgs) == 1
    assert notifier.msgs[0].title == "Daily commit digest"
    assert notifier.msgs[0].text == "THE DIGEST"


async def test_notifier_uses_configured_title(fake_llm) -> None:
    gh = FakeLister(by_repo={"o/r": [_commit("abc1234", "do the thing", "X")]})
    notifier = FakeNotifier()
    a = build_summary_agent(
        Deps(
            llm=fake_llm("D"), gh=gh, notify=notifier, repos=["o/r"],
            title="Weekly commit digest",
        )
    )
    runner = setup.new_runner("summary-test", a)
    await setup.drive(runner, "u", "s", "go")
    assert len(notifier.msgs) == 1
    assert notifier.msgs[0].title == "Weekly commit digest"


async def test_workflow_fetch_error(fake_llm) -> None:
    gh = FakeLister(error=RuntimeError("api down"))
    a = build_summary_agent(
        Deps(llm=fake_llm(""), gh=gh, notify=FakeNotifier(), repos=["o/r"])
    )
    runner = setup.new_runner("summary-test", a)
    # ParallelAgent runs fetchers in a task group, so a failing fetch surfaces as an
    # ExceptionGroup wrapping the RuntimeError.
    with pytest.raises((RuntimeError, BaseExceptionGroup)) as exc_info:
        await setup.drive(runner, "u", "s", "go")
    err = exc_info.value
    flat = (
        [str(e) for e in err.exceptions]  # type: ignore[attr-defined]
        if isinstance(err, BaseExceptionGroup)
        else [str(err)]
    )
    assert any("api down" in m for m in flat)
