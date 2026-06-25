"""Tests for fixflow apply-fix: a local seed git repo as the clone_url, a fake
GitHub capturing the created PR + labels, and the branch/commit/push/ensure_pr behavior
(plus existing-PR reuse and error paths).
"""

from __future__ import annotations

import pytest
from git import Actor
from git import Repo as GitRepo

from automation_agent.agent.fixflow.applyfix import ApplyConfig, FileEdit, apply_fix
from automation_agent.githubapi import PR, PRInput
from automation_agent.gitrepo import Author


class FakeGH:
    def __init__(self, existing: list[PR] | None = None, create_err: Exception | None = None) -> None:
        self.existing = existing or []
        self.created: PRInput | None = None
        self.labeled: list[str] = []
        self.create_err = create_err

    def find_open_pr_by_branch(self, owner: str, repo: str, branch: str) -> PR | None:
        return next((pr for pr in self.existing if pr.branch == branch), None)

    def create_pr(self, owner: str, repo: str, in_: PRInput) -> PR:
        if self.create_err is not None:
            raise self.create_err
        self.created = in_
        return PR(number=42, title=in_.title, branch=in_.head, head_sha="", url="https://gh/pr/42")

    def add_labels(self, owner: str, repo: str, number: int, *labels: str) -> None:
        self.labeled.extend(labels)


def _seed_remote(tmp_path) -> str:
    dir_ = tmp_path / "remote"
    dir_.mkdir()
    repo = GitRepo.init(dir_)
    (dir_ / "README.md").write_text("hi")
    repo.index.add(["README.md"])
    repo.index.commit("init", author=Actor("seed", "s@x"), committer=Actor("seed", "s@x"))
    return str(dir_)


def _apply_cfg(remote: str) -> ApplyConfig:
    return ApplyConfig(
        owner="acme", repo="api", clone_url=remote, token="", base="master",
        branch="agent/fix", new_branch=True, label="automation-agent",
        commit_message="fix", pr_title="Fix", pr_body="auto",
        author=Author(name="agent", email="a@x"),
    )


def test_apply_fix_creates_pr_and_pushes(tmp_path) -> None:
    remote = _seed_remote(tmp_path)
    gh = FakeGH()
    res = apply_fix(gh, _apply_cfg(remote), [FileEdit(path="internal/foo.go", content="package foo\n")])
    assert res.pr.number == 42
    assert res.head_sha != ""
    assert gh.created is not None and gh.created.head == "agent/fix"
    assert gh.labeled == ["automation-agent"]

    rr = GitRepo(remote)
    assert "agent/fix" in [h.name for h in rr.heads]


def test_apply_fix_retry_reuses_branch(tmp_path) -> None:
    remote = _seed_remote(tmp_path)
    apply_fix(FakeGH(), _apply_cfg(remote), [FileEdit(path="a.go", content="package a\n")])

    retry = _apply_cfg(remote)
    retry.new_branch = False
    gh = FakeGH(existing=[PR(number=9, title="", branch="agent/fix", head_sha="", url="")])
    res = apply_fix(gh, retry, [FileEdit(path="b.go", content="package b\n")])
    assert res.pr.number == 9
    assert gh.created is None  # reused, did not create


def test_apply_fix_no_edits() -> None:
    with pytest.raises(ValueError):
        apply_fix(FakeGH(), _apply_cfg("x"), [])


def test_apply_fix_clone_error(tmp_path) -> None:
    bad = _apply_cfg(str(tmp_path / "nope"))
    with pytest.raises(ValueError):
        apply_fix(FakeGH(), bad, [FileEdit(path="x.go", content="package x\n")])


def test_apply_fix_create_error(tmp_path) -> None:
    gh = FakeGH(create_err=TimeoutError("boom"))
    with pytest.raises(TimeoutError):
        apply_fix(gh, _apply_cfg(_seed_remote(tmp_path)), [FileEdit(path="x.go", content="package x\n")])


def test_apply_fix_rejects_path_escape(tmp_path) -> None:
    remote = _seed_remote(tmp_path)
    with pytest.raises(ValueError):
        apply_fix(FakeGH(), _apply_cfg(remote), [FileEdit(path="../escape.go", content="x")])
