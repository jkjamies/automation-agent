"""Tests for githubapi.

PyGithub uses urllib3/requests, which respx (httpx-based) cannot intercept, so we
instead:

  * fully test the pure ``parse_check_run_event``, and
  * monkeypatch ``client._gh`` with a PyGithub-shaped fake so the real logic paths
    (pagination/iteration, label filter, attempt count, check projection,
    not-found, file decode/directory) are exercised without network.
"""

from __future__ import annotations

import base64
from datetime import UTC, datetime
from types import SimpleNamespace

import pytest

from automation_agent.githubapi import (
    Client,
    PRInput,
    parse_check_run_event,
)

# --- PyGithub-shaped fakes ---------------------------------------------------


class FakePaginated(list):
    """Mimics PaginatedList: iterable + ``totalCount``."""

    @property
    def totalCount(self) -> int:  # noqa: N802 (matches PyGithub)
        return len(self)


def _commit(sha, message, author_name, when, html_url):
    author = SimpleNamespace(name=author_name, date=when)
    inner = SimpleNamespace(message=message, author=author)
    return SimpleNamespace(sha=sha, commit=inner, html_url=html_url)


def _pull(number, title, ref, sha, html_url, labels):
    head = SimpleNamespace(ref=ref, sha=sha)
    lbls = [SimpleNamespace(name=n) for n in labels]
    return SimpleNamespace(number=number, title=title, head=head, html_url=html_url, labels=lbls)


def _check_run(name, status, conclusion, started_at, completed_at, text=None, summary=None):
    output = SimpleNamespace(text=text, summary=summary)
    return SimpleNamespace(
        name=name,
        status=status,
        conclusion=conclusion,
        started_at=started_at,
        completed_at=completed_at,
        output=output,
    )


class FakeRepo:
    def __init__(self, **kw):
        self._kw = kw
        self.created_pull = None
        self.labeled = None

    def get_commits(self, since=None):
        self._kw["since_seen"] = since
        return FakePaginated(self._kw.get("commits", []))

    def create_pull(self, title, head, base, body):
        self.created_pull = dict(title=title, head=head, base=base, body=body)
        return self._kw["new_pull"]

    def get_issue(self, number):
        repo = self

        class _Issue:
            def add_to_labels(self_inner, *labels):
                repo.labeled = (number, list(labels))

        return _Issue()

    def get_pulls(self, state=None, head=None):
        self._kw["pulls_state"] = state
        self._kw["pulls_head"] = head
        return FakePaginated(self._kw.get("pulls", []))

    def get_pull(self, number):
        repo = self

        class _PR:
            def get_commits(self_inner):
                return FakePaginated(repo._kw.get("pr_commits", []))

        return _PR()

    def get_commit(self, ref):
        runs = self._kw.get("check_runs_by_ref", {}).get(ref, [])
        repo = self

        class _Commit:
            def get_check_runs(self_inner, check_name=None):
                repo._kw["check_name_seen"] = check_name
                return FakePaginated(runs)

        return _Commit()

    def get_contents(self, path, ref=None):
        self._kw["contents_ref"] = ref
        return self._kw["contents"]

    def compare(self, base, head):
        self._kw["compare_args"] = (base, head)
        return self._kw["comparison"]


def _comparison(total_commits, files):
    """files: list of (path, status, additions, deletions)."""
    fs = [SimpleNamespace(filename=p, status=s, additions=a, deletions=d) for (p, s, a, d) in files]
    return SimpleNamespace(total_commits=total_commits, files=fs)


class FakeGithub:
    def __init__(self, repo):
        self._repo = repo
        self.repo_full_name = None

    def get_repo(self, full_name):
        self.repo_full_name = full_name
        return self._repo


def make_client(repo: FakeRepo) -> tuple[Client, FakeGithub]:
    c = Client("")
    gh = FakeGithub(repo)
    c._gh = gh
    return c, gh


# --- list_commits_since ------------------------------------------------------


def test_list_commits_since() -> None:
    when = datetime(2026, 6, 19, 10, 0, 0, tzinfo=UTC)
    repo = FakeRepo(commits=[_commit("abc", "fix bug", "Jane", when, "https://gh/abc")])
    c, gh = make_client(repo)

    commits = c.list_commits_since("o", "r", datetime.fromtimestamp(0, tz=UTC))

    assert gh.repo_full_name == "o/r"
    assert len(commits) == 1
    got = commits[0]
    assert got.sha == "abc"
    assert got.author == "Jane"
    assert got.message == "fix bug"
    assert got.url == "https://gh/abc"
    assert got.when == when


# --- create_pr + add_labels --------------------------------------------------


def test_create_pr_and_labels() -> None:
    new_pull = _pull(5, "fix lint", "agent/fix", "deadbeef", "https://gh/pr/5", [])
    repo = FakeRepo(new_pull=new_pull)
    c, _ = make_client(repo)

    pr = c.create_pr("o", "r", PRInput(title="fix lint", head="agent/fix", base="main"))
    assert pr.number == 5
    assert pr.branch == "agent/fix"
    assert pr.head_sha == "deadbeef"
    assert pr.url == "https://gh/pr/5"
    assert repo.created_pull["title"] == "fix lint"
    assert repo.created_pull["base"] == "main"

    c.add_labels("o", "r", 5, "automation-agent")
    assert repo.labeled == (5, ["automation-agent"])


# --- find_open_pr_by_branch --------------------------------------------------


def test_find_open_pr_by_branch() -> None:
    repo = FakeRepo(pulls=[_pull(5, "", "agent/fix", "s5", "", ["automation-agent"])])
    c, _ = make_client(repo)

    pr = c.find_open_pr_by_branch("o", "r", "agent/fix")
    assert repo._kw["pulls_state"] == "open"
    assert repo._kw["pulls_head"] == "o:agent/fix"
    assert pr is not None
    assert pr.number == 5


def test_find_open_pr_by_branch_none() -> None:
    repo = FakeRepo(pulls=[])
    c, _ = make_client(repo)

    assert c.find_open_pr_by_branch("o", "r", "nope") is None


# --- attempt_count -----------------------------------------------------------


def test_attempt_count() -> None:
    repo = FakeRepo(pr_commits=[_commit("a", "", "", None, ""), _commit("b", "", "", None, "")])
    c, _ = make_client(repo)

    n = c.attempt_count("o", "r", 7)
    assert n == 2


# --- agent_check -------------------------------------------------------------


def test_agent_check_found() -> None:
    completed = datetime(2026, 6, 19, 11, 0, 0, tzinfo=UTC)
    repo = FakeRepo(
        check_runs_by_ref={
            "sha1": [
                _check_run(
                    "agent-lint-verify",
                    "completed",
                    "success",
                    None,
                    completed,
                    text="",
                    summary="all checks passed",
                )
            ]
        }
    )
    c, _ = make_client(repo)

    res = c.agent_check("o", "r", "sha1", "agent-lint-verify")
    assert res.found
    assert res.status == "completed"
    assert res.conclusion == "success"
    # text empty -> fall back to summary
    assert res.output_text == "all checks passed"
    assert res.completed_at == completed
    assert repo._kw["check_name_seen"] == "agent-lint-verify"


def test_agent_check_prefers_text_over_summary() -> None:
    repo = FakeRepo(
        check_runs_by_ref={
            "sha1": [
                _check_run(
                    "agent-lint-verify",
                    "completed",
                    "failure",
                    None,
                    None,
                    text="errcheck: unchecked error",
                    summary="ignored",
                )
            ]
        }
    )
    c, _ = make_client(repo)
    res = c.agent_check("o", "r", "sha1", "agent-lint-verify")
    assert res.output_text == "errcheck: unchecked error"


def test_agent_check_missing() -> None:
    repo = FakeRepo(check_runs_by_ref={"sha2": []})
    c, _ = make_client(repo)

    missing = c.agent_check("o", "r", "sha2", "agent-lint-verify")
    assert not missing.found
    assert missing.name == ""


# --- get_file_content --------------------------------------------------------


def test_get_file_content() -> None:
    encoded = base64.b64encode(b"package foo\n")
    fc = SimpleNamespace(decoded_content=base64.b64decode(encoded))
    repo = FakeRepo(contents=fc)
    c, _ = make_client(repo)

    got = c.get_file_content("o", "r", "internal/foo.go", "main")
    assert got == "package foo\n"
    assert repo._kw["contents_ref"] == "main"


def test_get_file_content_default_ref() -> None:
    fc = SimpleNamespace(decoded_content=b"hello\n")
    repo = FakeRepo(contents=fc)
    c, _ = make_client(repo)
    got = c.get_file_content("o", "r", "x", "")
    assert got == "hello\n"
    assert repo._kw["contents_ref"] is None


def test_get_file_content_directory() -> None:
    repo = FakeRepo(contents=[SimpleNamespace(), SimpleNamespace()])
    c, _ = make_client(repo)
    with pytest.raises(ValueError):
        c.get_file_content("o", "r", "internal", "")


# --- error wrapping ----------------------------------------------------------


def test_method_wraps_errors() -> None:
    class Boom(FakeGithub):
        def get_repo(self, full_name):
            raise RuntimeError("network down")

    c = Client("")
    c._gh = Boom(None)
    with pytest.raises(ValueError, match="list commits o/r"):
        c.list_commits_since("o", "r", datetime.now(tz=UTC))


# --- parse_check_run_event (pure) --------------------------------------------


def test_parse_check_run_event() -> None:
    body = b"""{
        "action":"completed",
        "check_run":{
            "name":"agent-lint-verify",
            "status":"completed",
            "conclusion":"failure",
            "head_sha":"sha123",
            "output":{"text":"errcheck: unchecked error"},
            "pull_requests":[{"number":12,"head":{"ref":"agent/fix"}}]
        },
        "repository":{"full_name":"acme/api"}
    }"""
    ev = parse_check_run_event(body)
    assert ev.action == "completed"
    assert ev.check_name == "agent-lint-verify"
    assert ev.conclusion == "failure"
    assert ev.head_sha == "sha123"
    assert ev.pr_number == 12
    assert ev.pr_branch == "agent/fix"
    assert ev.repo_full_name == "acme/api"
    assert ev.output_text == "errcheck: unchecked error"


def test_parse_check_run_event_summary_fallback() -> None:
    body = b'{"check_run":{"output":{"summary":"all good"}}}'
    ev = parse_check_run_event(body)
    assert ev.output_text == "all good"


def test_parse_check_run_event_missing_fields() -> None:
    ev = parse_check_run_event(b"{}")
    assert ev.action == ""
    assert ev.check_name == ""
    assert ev.pr_number == 0
    assert ev.pr_branch == ""
    assert ev.repo_full_name == ""
    assert ev.output_text == ""


def test_parse_check_run_event_invalid_json() -> None:
    with pytest.raises(ValueError):
        parse_check_run_event(b"not json")


def test_compare_projects_commits_and_files() -> None:
    cmp = _comparison(2, [("a.py", "modified", 3, 1), ("b.py", "added", 10, 0)])
    repo = FakeRepo(comparison=cmp)
    c, gh = make_client(repo)
    out = c.compare("acme", "api", "main", "agent/branch")
    assert gh.repo_full_name == "acme/api"
    assert repo._kw["compare_args"] == ("main", "agent/branch")
    assert out.total_commits == 2
    assert [f.path for f in out.files] == ["a.py", "b.py"]
    assert out.files[0].status == "modified" and out.files[0].additions == 3
    assert out.files[1].status == "added" and out.files[1].deletions == 0
