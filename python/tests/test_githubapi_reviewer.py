"""Tests for the githubapi reviewer methods.

PyGithub uses urllib3, which httpx-based mocks cannot intercept, so we monkeypatch
``client._gh`` with a PyGithub-shaped fake to exercise the real logic (projection, pagination,
the check-conclusion guard, the marker-upsert ownership + fallback, the GraphQL minimize) without
network. The pure ``parse_pull_request_event`` is tested directly.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from automation_agent.auth import StaticProvider
from automation_agent.githubapi import (
    CheckRunInput,
    Client,
    ReviewComment,
    ReviewInput,
    parse_pull_request_event,
)


def _client() -> Client:
    return Client(StaticProvider(""))


# --- parse_pull_request_event ------------------------------------------------


def test_parse_pull_request_event() -> None:
    body = (
        b'{"action":"synchronize","pull_request":{"number":7,"draft":true,'
        b'"head":{"ref":"feature/x","sha":"abc"},"base":{"ref":"main"},'
        b'"user":{"login":"dependabot[bot]"},"labels":[{"name":"skip-review"},{"name":"bug"}]},'
        b'"repository":{"full_name":"o/r"}}'
    )
    ev = parse_pull_request_event(body)
    assert ev.action == "synchronize" and ev.number == 7 and ev.repo_full_name == "o/r"
    assert ev.head_ref == "feature/x" and ev.head_sha == "abc" and ev.base_ref == "main"
    assert ev.draft is True and ev.author_login == "dependabot[bot]"
    assert ev.labels == ["skip-review", "bug"]


def test_parse_pull_request_event_defaults_and_malformed() -> None:
    ev = parse_pull_request_event(b"{}")
    assert ev.action == "" and ev.number == 0 and ev.labels == []
    with pytest.raises(ValueError):
        parse_pull_request_event(b"{bad")


# --- list_pr_files / head sha / tree -----------------------------------------


class _Repo:
    def __init__(self, **kw):
        self._kw = kw
        self.created_check = None
        self.created_comment = None

    def get_pull(self, number):
        return self._kw["pull"]

    def get_issue(self, number):
        return self._kw["issue"]

    def get_git_tree(self, sha, recursive=None):
        return self._kw["tree"]

    def create_check_run(self, **kw):
        self.created_check = kw


def _gh_with(repo) -> SimpleNamespace:
    return SimpleNamespace(get_repo=lambda full: repo)


def test_list_pr_files_projection() -> None:
    files = [
        SimpleNamespace(
            filename="a.go",
            previous_filename="",
            status="modified",
            additions=3,
            deletions=1,
            patch="@@",
        ),
        SimpleNamespace(
            filename="b.go",
            previous_filename="old.go",
            status="renamed",
            additions=0,
            deletions=0,
            patch=None,
        ),
    ]
    pull = SimpleNamespace(get_files=lambda: files, head=SimpleNamespace(sha="headsha"))
    c = _client()
    c._gh = _gh_with(_Repo(pull=pull))
    got = c.list_pr_files("o", "r", 1)
    assert [f.path for f in got] == ["a.go", "b.go"]
    assert got[1].previous_path == "old.go" and got[1].patch == ""
    assert c.pull_request_head_sha("o", "r", 1) == "headsha"


def test_tree_projection_and_truncation() -> None:
    tree = SimpleNamespace(
        tree=[SimpleNamespace(path="AGENTS.md", sha="s1", type="blob")],
        truncated=True,
    )
    c = _client()
    c._gh = _gh_with(_Repo(tree=tree))
    entries, truncated = c.tree("o", "r", "head")
    assert truncated is True and entries[0].path == "AGENTS.md" and entries[0].type == "blob"


# --- create_review / create_check_run ----------------------------------------


def test_create_review_maps_comments() -> None:
    captured = {}

    def create_review(**kw):
        captured.update(kw)

    pull = SimpleNamespace(create_review=create_review)
    c = _client()
    c._gh = _gh_with(_Repo(pull=pull))
    c.create_review(
        "o",
        "r",
        1,
        ReviewInput(
            body="hi", comments=[ReviewComment(path="a.go", line=2, side="RIGHT", body="x")]
        ),
    )
    assert captured["event"] == "COMMENT" and captured["body"] == "hi"
    assert captured["comments"] == [{"path": "a.go", "body": "x", "line": 2, "side": "RIGHT"}]


def test_create_check_run_conclusion_guard() -> None:
    repo = _Repo(pull=None)
    c = _client()
    c._gh = _gh_with(repo)
    c.create_check_run(
        "o",
        "r",
        CheckRunInput(
            name="agent-review", head_sha="s", conclusion="success", title="t", summary="u"
        ),
    )
    assert (
        repo.created_check["status"] == "completed"
        and repo.created_check["conclusion"] == "success"
    )
    with pytest.raises(ValueError):
        c.create_check_run(
            "o", "r", CheckRunInput(name="agent-review", head_sha="s", conclusion="failure")
        )


# --- list_review_comments / minimize -----------------------------------------


def test_list_review_comments_projection() -> None:
    comments = [SimpleNamespace(node_id="n1", body="b1"), SimpleNamespace(node_id="n2", body="b2")]
    pull = SimpleNamespace(get_review_comments=lambda: comments)
    c = _client()
    c._gh = _gh_with(_Repo(pull=pull))
    got = c.list_review_comments("o", "r", 1)
    assert [(g.node_id, g.body) for g in got] == [("n1", "b1"), ("n2", "b2")]


def test_minimize_comment_uses_graphql() -> None:
    calls = {}

    def graphql_query(query, variables):
        calls["query"] = query
        calls["variables"] = variables
        return {}, {}

    c = _client()
    c._gh = SimpleNamespace(requester=SimpleNamespace(graphql_query=graphql_query))
    c.minimize_comment("NODE123")
    assert "minimizeComment" in calls["query"] and "OUTDATED" in calls["query"]
    assert calls["variables"] == {"id": "NODE123"}


# --- upsert_marker_comment ---------------------------------------------------


class _Comment:
    def __init__(self, body, login="me", ctype="User", edit_error=None):
        self.body = body
        self.user = SimpleNamespace(login=login, type=ctype)
        self._edit_error = edit_error
        self.edited = None

    def edit(self, body):
        if self._edit_error is not None:
            raise self._edit_error
        self.edited = body


class _Issue:
    def __init__(self, comments):
        self._comments = comments
        self.created = None

    def get_comments(self):
        return self._comments

    def create_comment(self, body):
        self.created = body


def test_upsert_marker_requires_marker() -> None:
    c = _client()
    with pytest.raises(ValueError):
        c.upsert_marker_comment("o", "r", 1, "", "body")
    with pytest.raises(ValueError):
        c.upsert_marker_comment("o", "r", 1, "MARK", "no marker in body")


def test_upsert_marker_edits_matching_comment() -> None:
    marker = "<!-- mark -->"
    existing = _Comment(body="old " + marker)
    issue = _Issue([existing])
    c = _client()
    c._gh = _gh_with(_Repo(issue=issue))
    c.upsert_marker_comment("o", "r", 1, marker, "new " + marker)
    assert existing.edited == "new " + marker and issue.created is None


def test_upsert_marker_creates_when_none() -> None:
    marker = "<!-- mark -->"
    issue = _Issue([_Comment(body="unrelated")])
    c = _client()
    c._gh = _gh_with(_Repo(issue=issue))
    c.upsert_marker_comment("o", "r", 1, marker, "new " + marker)
    assert issue.created == "new " + marker


def test_upsert_marker_app_authored_only_edits_bot() -> None:
    marker = "<!-- mark -->"
    human = _Comment(body="human " + marker, ctype="User")
    issue = _Issue([human])
    c = Client(StaticProvider(""), app_authored=True)
    c._gh = _gh_with(_Repo(issue=issue))
    c.upsert_marker_comment("o", "r", 1, marker, "new " + marker)
    # The human comment is not owned in App mode -> a fresh comment is created instead.
    assert human.edited is None and issue.created == "new " + marker


def test_upsert_marker_403_fallthrough_creates() -> None:
    marker = "<!-- mark -->"
    err = SimpleNamespace()  # not an HTTP error -> re-raised... use a real status carrier
    forbidden = _Comment(body="foreign " + marker, edit_error=_http_error(403))
    issue = _Issue([forbidden])
    c = _client()  # no authored_login -> weak fallback
    c._gh = _gh_with(_Repo(issue=issue))
    c.upsert_marker_comment("o", "r", 1, marker, "new " + marker)
    assert issue.created == "new " + marker
    _ = err


def _http_error(status: int) -> Exception:
    e = RuntimeError("boom")
    e.status = status  # type: ignore[attr-defined]
    return e
