"""Deterministic tests for the reviewer engine: the intake decision matrix, the kickoff path,
coalesce-to-latest staleness, the model-calling review pipeline (canned findings), and the enqueue
coalescing hints.

A fake GitHub client returns canned changed files; a scripted ``FakeLlm`` returns canned JSON. We
never assert on real LLM output — only orchestration and deterministic logic.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

import pytest

from automation_agent.agent import reviewer
from automation_agent.agent.reviewer.enqueue import coalesce_key, enqueue_options
from automation_agent.agent.reviewer.review import format_diff, run_review
from automation_agent.agent.reviewer.reviewer import split_full_name
from automation_agent.agent.reviewer.scorecard import Level
from automation_agent.githubapi import PRFile, PullRequestEvent
from automation_agent.ingest import Kind, new
from tests.conftest import FakeLlm


class FakeGH:
    """A stub GitHub client for the analysis half: returns canned changed files (or raises) and
    reports a head SHA (for the staleness check)."""

    def __init__(
        self,
        files: list[PRFile] | None = None,
        *,
        raise_list: Exception | None = None,
        head_sha: str = "",
        head_sha_error: Exception | None = None,
    ) -> None:
        self.files = files or []
        self._raise_list = raise_list
        self._head_sha = head_sha
        self._head_sha_error = head_sha_error
        self.calls = 0

    def list_pr_files(self, owner, repo, number):
        self.calls += 1
        if self._raise_list is not None:
            raise self._raise_list
        return self.files

    def pull_request_head_sha(self, owner, repo, number):
        if self._head_sha_error is not None:
            raise self._head_sha_error
        return self._head_sha


def _engine(gh, *, canned: str = "[]", **overrides) -> reviewer.Engine:
    llm = FakeLlm(canned)
    deps = reviewer.Deps(
        enabled=True,
        gh=gh,
        base_llm=llm,
        code_llm=llm,
        min_confidence=0.6,
        skip_drafts=True,
        exclude_globs=["go.sum", "vendor/**"],
        max_files=50,
        max_diff_bytes=1000,
    )
    for k, v in overrides.items():
        setattr(deps, k, v)
    return reviewer.new_engine(deps)


def _event(action: str, **kw) -> PullRequestEvent:
    ev = PullRequestEvent(action=action, number=1, repo_full_name="o/r", head_ref="feature/x")
    for k, v in kw.items():
        setattr(ev, k, v)
    return ev


# --- decide matrix -----------------------------------------------------------


def test_decide_matrix() -> None:
    real = [PRFile(path="main.go", patch="abc")]

    gh = FakeGH(real)
    assert _engine(gh).decide(_event("closed")).kind is reviewer.DecisionKind.SKIP
    assert gh.calls == 0  # untriggered action skips before fetch

    gh = FakeGH(real)
    assert _engine(gh).decide(_event("opened", draft=True)).kind is reviewer.DecisionKind.SKIP
    assert gh.calls == 0  # draft skipped pre-fetch

    assert (
        _engine(FakeGH(real)).decide(_event("ready_for_review", draft=True)).kind
        is reviewer.DecisionKind.REVIEW
    )
    assert (
        _engine(FakeGH(real)).decide(_event("opened", head_ref="automation-agent/lint")).kind
        is reviewer.DecisionKind.SKIP
    )
    assert (
        _engine(FakeGH(real)).decide(_event("opened", labels=["skip-review"])).kind
        is reviewer.DecisionKind.SKIP
    )
    assert (
        _engine(FakeGH(real)).decide(_event("opened", author_login="dependabot[bot]")).kind
        is reviewer.DecisionKind.SKIP
    )

    # all-excluded -> skip after fetch
    gh = FakeGH([PRFile(path="go.sum", patch="x"), PRFile(path="vendor/y.go", patch="x")])
    d = _engine(gh).decide(_event("opened"))
    assert d.kind is reviewer.DecisionKind.SKIP and gh.calls == 1

    # normal PR reviews on the filtered size (go.sum excluded)
    gh = FakeGH([PRFile(path="main.go", patch="12345"), PRFile(path="go.sum", patch="ignored")])
    d = _engine(gh).decide(_event("synchronize"))
    assert d.kind is reviewer.DecisionKind.REVIEW and len(d.files) == 1 and d.diff_bytes == 5

    # oversize -> deny
    gh = FakeGH([PRFile(path="a.go", patch="x"), PRFile(path="b.go", patch="x")])
    d = _engine(gh, max_files=1).decide(_event("opened"))
    assert d.kind is reviewer.DecisionKind.DENY and d.reason


def test_decide_malformed_name_and_list_error() -> None:
    with pytest.raises(ValueError):
        _engine(FakeGH([PRFile(path="main.go", patch="x")])).decide(
            _event("opened", repo_full_name="noslash")
        )
    with pytest.raises(ValueError):
        _engine(FakeGH(raise_list=RuntimeError("boom"))).decide(_event("opened"))


def test_split_full_name() -> None:
    assert split_full_name("o/r") == ("o", "r", True)
    for bad in ("noslash", "a/b/c", "/r", "o/"):
        assert split_full_name(bad)[2] is False


# --- kickoff -----------------------------------------------------------------


async def test_kickoff_disabled_noop() -> None:
    gh = FakeGH()
    e = reviewer.new_engine(reviewer.Deps(enabled=False, gh=gh))
    await e.kickoff(b"not even json")
    assert gh.calls == 0


async def test_kickoff_enabled_nil_client_errors() -> None:
    e = reviewer.new_engine(reviewer.Deps(enabled=True, gh=None))
    body = b'{"action":"opened","pull_request":{"number":1,"head":{"ref":"x"}},"repository":{"full_name":"o/r"}}'
    with pytest.raises(ValueError):
        await e.kickoff(body)


async def test_kickoff_malformed_body_errors() -> None:
    with pytest.raises(ValueError):
        await _engine(FakeGH()).kickoff(b"{bad")


async def test_kickoff_review_path() -> None:
    canned = '[{"file":"main.go","line":1,"dimension":"performance","severity":"medium","message":"slow","confidence":0.9}]'
    gh = FakeGH([PRFile(path="main.go", patch="@@\n+x", status="modified")])
    e = _engine(gh, canned=canned)
    body = b'{"action":"opened","pull_request":{"number":7,"head":{"ref":"feature/x"},"base":{"ref":"main"}},"repository":{"full_name":"o/r"}}'
    await e.kickoff(body)
    assert gh.calls == 1 and e.base_llm.requests  # fetched, then ran the review pipeline


async def test_kickoff_staleness() -> None:
    def body(sha: str) -> bytes:
        return (
            '{"action":"synchronize","pull_request":{"number":3,"head":{"ref":"x","sha":"'
            + sha
            + '"},"base":{"ref":"main"}},"repository":{"full_name":"o/r"}}'
        ).encode()

    real = [PRFile(path="main.go", patch="@@ -1 +1 @@\n+x")]

    gh = FakeGH(real, head_sha="newsha")
    e = _engine(gh)
    await e.kickoff(body("oldsha"))
    assert not e.base_llm.requests  # stale -> review did not run

    gh = FakeGH(real, head_sha="samesha")
    e = _engine(gh)
    await e.kickoff(body("samesha"))
    assert e.base_llm.requests  # current -> review ran

    gh = FakeGH(real, head_sha="newsha", head_sha_error=RuntimeError("boom"))
    e = _engine(gh)
    await e.kickoff(body("oldsha"))
    assert e.base_llm.requests  # lookup error is best-effort, proceeds


# --- review pipeline ---------------------------------------------------------


async def test_review_pipeline_dedup_and_gate() -> None:
    canned = '[{"file":"main.go","line":10,"dimension":"runtime_safety","severity":"major","message":"nil deref","confidence":0.9}]'
    files = [PRFile(path="main.go", patch="@@ -1 +1 @@\n+x", status="modified")]

    card, _ = await run_review(_engine(FakeGH(), canned=canned), files)
    # Every lens + glue returns the same fingerprint -> dedup to one; one runtime_safety major
    # scores yellow.
    assert card.total == 1 and card.overall is Level.YELLOW


async def test_review_pipeline_drops_low_confidence_and_empty() -> None:
    files = [PRFile(path="main.go", patch="+x")]
    low = '[{"file":"main.go","line":10,"dimension":"security","severity":"critical","message":"x","confidence":0.2}]'
    card, _ = await run_review(_engine(FakeGH(), canned=low), files)
    assert card.total == 0 and card.overall is Level.GREEN

    card, _ = await run_review(_engine(FakeGH(), canned="[]"), files)
    assert card.total == 0 and card.overall is Level.GREEN


def test_format_diff() -> None:
    out = format_diff(
        [
            PRFile(path="a.go", status="modified", patch="@@ -1 +1 @@\n-old\n+new"),
            PRFile(path="logo.png", status="added", patch=""),
        ]
    )
    assert "### a.go (modified)" in out and "+new" in out
    assert "### logo.png (added)" in out and "(no textual diff available)" in out


# --- enqueue -----------------------------------------------------------------


def _review_envelope(action: str, at: datetime | None = None):
    at = at or datetime.fromtimestamp(1_700_000_000, tz=UTC)
    body = (
        '{"action":"' + action + '","pull_request":{"number":7,"head":{"ref":"x","sha":"s"}},'
        '"repository":{"full_name":"acme/web.api"}}'
    ).encode()
    return new(Kind.REVIEW, "webhook:/github", body, at)


def test_enqueue_synchronize_debounces() -> None:
    opts = enqueue_options(_review_envelope("synchronize"), timedelta(seconds=30))
    assert opts["delay"] == timedelta(seconds=30)
    assert opts["name"].startswith("review-") and "-7-" in opts["name"]


def test_enqueue_buckets_by_window() -> None:
    base = datetime.fromtimestamp(1_700_000_000, tz=UTC)
    a = enqueue_options(
        _review_envelope("synchronize", base + timedelta(seconds=2)), timedelta(seconds=30)
    )
    b = enqueue_options(
        _review_envelope("synchronize", base + timedelta(seconds=5)), timedelta(seconds=30)
    )
    c = enqueue_options(
        _review_envelope("synchronize", base + timedelta(seconds=45)), timedelta(seconds=30)
    )
    assert a["name"] == b["name"]  # same window coalesces
    assert a["name"] != c["name"]  # later window distinct


def test_coalesce_key_lossless_no_collision() -> None:
    a = coalesce_key(PullRequestEvent(repo_full_name="acme/web.api", number=7), 1_700_000_000)
    b = coalesce_key(PullRequestEvent(repo_full_name="acme/web-api", number=7), 1_700_000_000)
    assert a != b


def test_enqueue_immediate_and_disabled() -> None:
    for action in ("opened", "reopened", "ready_for_review"):
        assert enqueue_options(_review_envelope(action), timedelta(seconds=30)) == {}
    assert enqueue_options(_review_envelope("synchronize"), timedelta(0)) == {}
    ci = new(Kind.CI, "webhook:/github", b"{}", datetime.now(UTC))
    assert enqueue_options(ci, timedelta(seconds=30)) == {}
    bad = new(Kind.REVIEW, "webhook:/github", b"{not json", datetime.now(UTC))
    assert enqueue_options(bad, timedelta(seconds=30)) == {}
