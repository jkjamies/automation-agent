"""Tests for gitrepo.

Uses a LOCAL seed repo (no network). A temp bare/non-bare
"remote" is seeded with one commit, then cloned, and clone/checkout/commit_all/
push/head/checkout_remote are exercised against it.
"""

from __future__ import annotations

import os

import pytest
from git import Repo as GitRepo

from automation_agent.gitrepo import Auth, Author, NoChangesError, Repo


class _FakeProvider:
    """A gitrepo.TokenProvider that yields a fixed token and records its calls, so tests
    can assert the per-op token lookup happened (or, for insecure remotes, did NOT)."""

    def __init__(self, token: str = "") -> None:
        self._token = token
        self.calls: list[str] = []

    def token(self, repo: str) -> str:
        self.calls.append(repo)
        return self._token


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

    r = Repo.clone(remote, work)

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
    r1 = Repo.clone(remote, str(tmp_path / "w1"))
    r1.checkout("feature", create=True)
    with open(r1.path("f.txt"), "w") as f:
        f.write("x")
    sha = r1.commit_all("feat", Author(name="a", email="a@x"))
    r1.push()

    # Second clone: check out the existing remote branch.
    r2 = Repo.clone(remote, str(tmp_path / "w2"))
    r2.checkout_remote("feature")
    head = r2.head()
    assert head == sha

    with pytest.raises(ValueError):
        r2.checkout_remote("does-not-exist")


def test_checkout_missing_branch(tmp_path) -> None:
    r = Repo.clone(seed_remote(tmp_path), str(tmp_path / "w"))
    with pytest.raises(ValueError):
        r.checkout("does-not-exist", create=False)


def test_commit_nothing(tmp_path) -> None:
    r = Repo.clone(seed_remote(tmp_path), str(tmp_path / "w"))
    with pytest.raises(NoChangesError):
        r.commit_all("nothing changed", Author(name="a", email="a@x"))


def test_clone_bad_url(tmp_path) -> None:
    work = str(tmp_path / "nope")
    with pytest.raises(ValueError):
        Repo.clone(str(tmp_path / "does-not-exist"), work)


def test_auth_url_embeds_token(tmp_path) -> None:
    # token embeds x-access-token for https; local paths untouched.
    from automation_agent.gitrepo.repo import _auth_url

    assert _auth_url("https://github.com/o/r.git", "tok") == (
        "https://x-access-token:tok@github.com/o/r.git"
    )
    local = os.path.join(str(tmp_path), "repo")
    assert _auth_url(local, "tok") == local
    assert _auth_url("https://github.com/o/r.git", "") == "https://github.com/o/r.git"
    # An ssh remote carries no in-URL credential — returned unchanged even with a token.
    assert _auth_url("git@github.com:o/r.git", "tok") == "git@github.com:o/r.git"


def test_is_ssh_url() -> None:
    from automation_agent.gitrepo.repo import _is_ssh_url

    assert _is_ssh_url("git@github.com:acme/api.git")
    assert _is_ssh_url("ssh://git@github.com/acme/api.git")
    assert not _is_ssh_url("https://github.com/acme/api.git")
    assert not _is_ssh_url("/local/path/repo")


def test_ssh_env() -> None:
    from automation_agent.gitrepo.repo import _ssh_env

    assert _ssh_env("") is None
    assert _ssh_env("/home/dev/.ssh/id_ed25519") == {
        "GIT_SSH_COMMAND": "ssh -i /home/dev/.ssh/id_ed25519 -o IdentitiesOnly=yes"
    }
    # A path with spaces is shell-quoted so git invokes ssh with the intended key.
    env = _ssh_env("/home/my key/id")
    assert env is not None
    assert "'/home/my key/id'" in env["GIT_SSH_COMMAND"]


def test_clone_threads_ssh_command(tmp_path, monkeypatch) -> None:
    # An ssh URL with GIT_SSH_KEY pins ssh to that key via GIT_SSH_COMMAND; an https or local
    # URL passes no ssh env. Captured without touching the network by stubbing clone_from.
    import automation_agent.gitrepo.repo as repomod

    captured: dict[str, object] = {}

    class FakeGit:
        def __init__(self) -> None:
            self.persisted: dict[str, str] = {}

        def update_environment(self, **env: str) -> None:
            self.persisted.update(env)

    class FakeRepo:
        def __init__(self) -> None:
            self.git = FakeGit()

    def fake_clone_from(url, dir, env=None):
        captured["url"] = url
        captured["env"] = env
        repo = FakeRepo()
        captured["repo"] = repo
        return repo

    monkeypatch.setattr(repomod.GitRepo, "clone_from", fake_clone_from)

    Repo.clone(
        "git@github.com:acme/api.git", str(tmp_path / "w1"), Auth(ssh_key="/k/id_ed25519")
    )
    assert captured["env"] == {
        "GIT_SSH_COMMAND": "ssh -i /k/id_ed25519 -o IdentitiesOnly=yes"
    }
    # clone_from's env is subprocess-scoped, so the same command must be persisted onto the
    # returned repo's Git instance — otherwise a later push() over ssh would drop the key.
    assert captured["repo"].git.persisted == {
        "GIT_SSH_COMMAND": "ssh -i /k/id_ed25519 -o IdentitiesOnly=yes"
    }

    # An https URL ignores ssh_key (no ssh env), resolves a token from the provider, and
    # keeps the token-embedded URL.
    prov = _FakeProvider("tok")
    Repo.clone(
        "https://github.com/acme/api.git",
        str(tmp_path / "w2"),
        Auth(provider=prov, repo="acme/api", ssh_key="/k/id"),
    )
    assert captured["env"] is None
    assert captured["url"] == "https://x-access-token:tok@github.com/acme/api.git"
    assert prov.calls == ["acme/api"]  # per-op, repo-scoped lookup happened.

    # An ssh URL with no explicit key: no env, so system git resolves creds (ssh-agent etc).
    Repo.clone("git@github.com:acme/api.git", str(tmp_path / "w3"), Auth())
    assert captured["env"] is None


def test_token_for_refuses_insecure_http() -> None:
    # Sending a PAT/App token as basic auth over plaintext http:// would leak it; the token
    # lookup must be refused outright and the provider never consulted.
    from automation_agent.gitrepo.repo import _token_for

    prov = _FakeProvider("tok")
    with pytest.raises(ValueError, match="insecure http"):
        _token_for("http://github.example/acme/api.git", Auth(provider=prov, repo="acme/api"))
    assert prov.calls == []  # no token minted for the rejected remote.


def test_token_for_skips_local_and_ssh() -> None:
    # Local / file / ssh remotes need no token — the provider is not consulted (avoids a
    # needless App installation-token mint).
    from automation_agent.gitrepo.repo import _token_for

    prov = _FakeProvider("tok")
    assert _token_for("/local/path/repo", Auth(provider=prov, repo="o/r")) == ""
    assert _token_for("git@github.com:o/r.git", Auth(provider=prov, repo="o/r")) == ""
    assert prov.calls == []
    # https with a provider does resolve a token.
    assert _token_for("https://github.com/o/r.git", Auth(provider=prov, repo="o/r")) == "tok"
    assert prov.calls == ["o/r"]
