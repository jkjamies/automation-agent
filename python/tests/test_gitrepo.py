"""Tests for gitrepo.

Uses a LOCAL seed repo (no network). A temp bare/non-bare
"remote" is seeded with one commit, then cloned, and clone/checkout/commit_all/
push/head/checkout_remote are exercised against it.
"""

from __future__ import annotations

import os

import pytest
from git import Repo as GitRepo

from automation_agent.gitrepo import Author, NoChangesError, Repo


def seed_remote(tmp_path) -> str:
    """Create a local repo with one commit to act as the clone source."""
    d = tmp_path / "remote"
    d.mkdir()
    repo = GitRepo.init(str(d))
    (d / "README.md").write_text("hi")
    repo.index.add(["README.md"])
    repo.index.commit("init")
    # Push targets a checked-out branch; allow it so Push() succeeds against a
    # non-bare remote.
    repo.git.config("receive.denyCurrentBranch", "ignore")
    return str(d)


def test_clone_branch_commit_push(tmp_path) -> None:
    remote = seed_remote(tmp_path)
    work = str(tmp_path / "work")

    r = Repo.clone(remote, work, "")

    r.checkout("agent/fix", create=True)
    with open(r.path("fix.txt"), "w") as f:
        f.write("patched")

    sha = r.commit_all("apply fix", Author(name="agent", email="a@x"))
    head = r.head()
    assert head == sha

    assert r.dir() == work

    r.push()
    # A second push with no new commits is up-to-date, not an error.
    r.push()

    # The remote should now have the pushed branch at the committed SHA.
    rr = GitRepo(remote)
    ref = rr.refs["agent/fix"]
    assert ref.commit.hexsha == sha


def test_checkout_remote(tmp_path) -> None:
    remote = seed_remote(tmp_path)

    # First clone: create and push a branch.
    r1 = Repo.clone(remote, str(tmp_path / "w1"), "")
    r1.checkout("feature", create=True)
    with open(r1.path("f.txt"), "w") as f:
        f.write("x")
    sha = r1.commit_all("feat", Author(name="a", email="a@x"))
    r1.push()

    # Second clone: check out the existing remote branch.
    r2 = Repo.clone(remote, str(tmp_path / "w2"), "")
    r2.checkout_remote("feature")
    head = r2.head()
    assert head == sha

    with pytest.raises(ValueError):
        r2.checkout_remote("does-not-exist")


def test_checkout_missing_branch(tmp_path) -> None:
    r = Repo.clone(seed_remote(tmp_path), str(tmp_path / "w"), "")
    with pytest.raises(ValueError):
        r.checkout("does-not-exist", create=False)


def test_commit_nothing(tmp_path) -> None:
    r = Repo.clone(seed_remote(tmp_path), str(tmp_path / "w"), "")
    with pytest.raises(NoChangesError):
        r.commit_all("nothing changed", Author(name="a", email="a@x"))


def test_clone_bad_url(tmp_path) -> None:
    work = str(tmp_path / "nope")
    with pytest.raises(ValueError):
        Repo.clone(str(tmp_path / "does-not-exist"), work, "")


def test_auth_url_embeds_token(tmp_path) -> None:
    # token embeds x-access-token for https; local paths untouched.
    from automation_agent.gitrepo.repo import _auth_url

    assert _auth_url("https://github.com/o/r.git", "tok") == (
        "https://x-access-token:tok@github.com/o/r.git"
    )
    local = os.path.join(str(tmp_path), "repo")
    assert _auth_url(local, "tok") == local
    assert _auth_url("https://github.com/o/r.git", "") == "https://github.com/o/r.git"
