"""Working-tree git operations the lint-fixer needs: clone, branch, stage-all,
commit, push — via GitPython.

Deterministic tooling — no agent imports.

Functions return a value and RAISE on error. A clean working tree raises the sentinel
:class:`NoChangesError`. GitPython is synchronous, so there is no request-context
parameter to plumb through.
"""

from __future__ import annotations

import os
import shlex
from dataclasses import dataclass
from urllib.parse import urlsplit, urlunsplit

from git import Actor
from git import Repo as GitRepo


@dataclass
class Author:
    """Identifies the committer."""

    name: str
    email: str


class NoChangesError(Exception):
    """Raised by :meth:`Repo.commit_all` when the working tree is clean (the edits
    produced no actual change), so callers can distinguish "nothing to do" from a
    real failure.
    """


def _auth_url(url: str, token: str) -> str:
    """Embed ``x-access-token:<token>@`` into https URLs for basic auth. Non-https
    (ssh / local path / file) remotes are returned unchanged — an ssh remote carries no
    in-URL credential; system ``git`` authenticates it via ssh-agent / default keys.
    """
    if not token:
        return url
    parts = urlsplit(url)
    if parts.scheme not in ("http", "https"):
        return url
    host = parts.hostname or ""
    if parts.port is not None:
        host = f"{host}:{parts.port}"
    netloc = f"x-access-token:{token}@{host}"
    return urlunsplit((parts.scheme, netloc, parts.path, parts.query, parts.fragment))


def _is_ssh_url(url: str) -> bool:
    """Whether url is an scp-style (``git@host:path``) or ``ssh://`` remote, as opposed to
    an https remote. The agent only ever builds these two forms (selected by GIT_TRANSPORT).
    """
    return url.startswith("ssh://") or url.startswith("git@")


def _ssh_env(ssh_key: str) -> dict[str, str] | None:
    """Build the git environment that pins ssh transport to an explicit private key via
    ``GIT_SSH_COMMAND``, or None when no key is configured.

    With no override, system ``git`` resolves ssh credentials itself (ssh-agent, then the
    default identity files), with ``known_hosts`` verification on. ``IdentitiesOnly=yes``
    stops ssh from also offering agent/other keys when an explicit key is given.
    """
    if not ssh_key:
        return None
    return {"GIT_SSH_COMMAND": f"ssh -i {shlex.quote(ssh_key)} -o IdentitiesOnly=yes"}


class Repo:
    """A cloned working tree."""

    def __init__(self, repo: GitRepo, dir_: str) -> None:
        self._repo = repo
        self._dir = dir_

    @staticmethod
    def clone(url: str, dir: str, token: str = "", ssh_key: str = "") -> Repo:
        """Clone url into dir (which must not already exist). Auth is chosen by the URL
        scheme: a non-empty token is embedded as GitHub HTTP auth for https URLs; an ssh
        URL (``git@…``/``ssh://…``) is left untouched so system ``git`` authenticates it
        via ssh-agent / default keys. A non-empty ``ssh_key`` pins ssh to that private key
        (via ``GIT_SSH_COMMAND``); it is ignored for https URLs.

        The ssh environment is carried onto the resulting repo by GitPython, so a later
        :meth:`push` over ssh reuses the same key.
        """
        clone_url = _auth_url(url, token)
        env = _ssh_env(ssh_key) if _is_ssh_url(url) else None
        try:
            repo = GitRepo.clone_from(clone_url, dir, env=env)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"clone {url}: {exc}") from exc
        return Repo(repo, dir)

    def dir(self) -> str:
        """Return the working-tree directory; callers write file edits under it."""
        return self._dir

    def path(self, rel: str) -> str:
        """Join rel onto the working-tree directory."""
        return os.path.join(self._dir, rel)

    def checkout(self, branch: str, create: bool = False) -> None:
        """Switch to branch, creating it from the current HEAD when create is True."""
        try:
            if create:
                self._repo.git.checkout("-b", branch)
            else:
                self._repo.git.checkout(branch)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"checkout {branch}: {exc}") from exc

    def checkout_remote(self, branch: str) -> None:
        """Check out an existing remote branch (origin/<branch>) as a local branch
        — used on retry iterations to add a commit onto the previous fix rather
        than starting a new branch from the base.
        """
        remote_ref = f"origin/{branch}"
        try:
            ref = self._repo.refs[remote_ref]
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"resolve origin/{branch}: {exc}") from exc
        try:
            local = self._repo.create_head(branch, ref.commit)
            local.checkout()
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"checkout {branch}: {exc}") from exc

    def commit_all(self, msg: str, author: Author) -> str:
        """Stage every change (including deletions) and commit, returning the new
        commit SHA. Raise :class:`NoChangesError` if the tree is clean.

        Invariant: one commit per attempt.
        """
        try:
            self._repo.git.add("--all")
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"stage changes: {exc}") from exc
        if not self._repo.is_dirty(index=True, working_tree=True, untracked_files=True):
            raise NoChangesError("gitrepo: no changes to commit")
        actor = Actor(author.name, author.email)
        # GitPython defaults both the author and commit timestamps to the current
        # time when none is supplied.
        try:
            commit = self._repo.index.commit(msg, author=actor)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"commit: {exc}") from exc
        return commit.hexsha

    def push(self) -> None:
        """Push the current branch to origin. An up-to-date push is not an error."""
        try:
            origin = self._repo.remote("origin")
            branch = self._repo.active_branch.name
            results = origin.push(refspec=f"{branch}:{branch}")
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"push: {exc}") from exc
        for info in results:
            if info.flags & info.ERROR:
                raise ValueError(f"push: {info.summary}")

    def head(self) -> str:
        """Return the current HEAD commit SHA."""
        try:
            return self._repo.head.commit.hexsha
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"head: {exc}") from exc
