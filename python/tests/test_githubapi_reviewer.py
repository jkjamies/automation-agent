"""Tests for the githubapi reviewer read methods.

PyGithub uses urllib3, which httpx-based mocks cannot intercept, so we monkeypatch
``client._gh`` with a PyGithub-shaped fake to exercise the real logic (projection, pagination)
without network. The pure ``parse_pull_request_event`` is tested directly.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from automation_agent.auth import StaticProvider
from automation_agent.githubapi import Client, parse_pull_request_event


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


# --- list_pr_files / head sha / tree / list_review_comments ------------------


class _Repo:
    def __init__(self, **kw):
        self._kw = kw

    def get_pull(self, number):
        return self._kw["pull"]

    def get_git_tree(self, sha, recursive=None):
        return self._kw["tree"]


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


def test_list_review_comments_projection() -> None:
    comments = [SimpleNamespace(node_id="n1", body="b1"), SimpleNamespace(node_id="n2", body="b2")]
    pull = SimpleNamespace(get_review_comments=lambda: comments)
    c = _client()
    c._gh = _gh_with(_Repo(pull=pull))
    got = c.list_review_comments("o", "r", 1)
    assert [(g.node_id, g.body) for g in got] == [("n1", "b1"), ("n2", "b2")]
