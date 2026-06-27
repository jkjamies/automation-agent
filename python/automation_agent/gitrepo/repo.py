"""Working-tree git operations the lint-fixer needs: clone, branch, stage-all,
commit, push — via GitPython.

Deterministic tooling — no agent imports.

Functions return a value and RAISE on error. A clean working tree raises the sentinel
:class:`NoChangesError`. GitPython is synchronous, so there is no request-context
parameter to plumb through.
"""

from __future__ import annotations

import contextlib
import os
import shlex
from dataclasses import dataclass
from typing import Protocol
from urllib.parse import urlsplit, urlunsplit

from git import Actor
from git import Repo as GitRepo


@dataclass
class Author:
    """Identifies the committer."""

    name: str
    email: str


class TokenProvider(Protocol):
    """Yields a valid GitHub token for a repo (``"owner/name"``), re-fetched per git op.
    The gitrepo-local view of ``auth.TokenProvider`` (a narrow protocol kept here so
    gitrepo stays decoupled from the ``auth`` package; structural typing matches the real
    providers)."""

    def token(self, repo: str) -> str: ...


@dataclass
class Auth:
    """Credentials Clone/Push use. Which one applies is chosen by the clone URL scheme,
    not by the caller: an https remote uses ``provider`` (GitHub ``x-access-token`` basic
    auth, re-fetched per op so a short-lived installation token stays current), an ssh
    remote (``git@…`` / ``ssh://…``) uses ``ssh_key`` or the ssh-agent."""

    # provider yields the token embedded as x-access-token basic auth on https remotes,
    # fetched fresh per git op (scoped to ``repo``). None — or a token of "" — means
    # anonymous (public read only). Ignored for ssh remotes.
    provider: TokenProvider | None = None
    # repo is "owner/name", passed to provider so App mode can scope the token.
    repo: str = ""
    # ssh_key is an explicit private-key path for ssh remotes; empty falls back to the
    # ssh-agent then default identities. Ignored for https remotes.
    ssh_key: str = ""


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


def _token_for(url: str, auth: Auth) -> str:
    """Resolve the token for an https git op, fetched fresh per op so a short-lived
    installation token stays current. Returns ``""`` (anonymous) for ssh / local / file
    remotes, which need no token — fetching one would needlessly mint a GitHub
    installation token in App mode.

    Raises:
        ValueError: for a plaintext ``http://`` remote — sending a PAT/App token as basic
            auth over an unencrypted transport would leak it; use https or ssh.
    """
    if _is_ssh_url(url):
        return ""
    if url.startswith("http://"):
        raise ValueError(
            "refusing to send GitHub token over insecure http remote; use https or ssh"
        )
    if not url.startswith("https://"):
        return ""  # local path / file:// — no credentials.
    if auth.provider is None:
        return ""
    return auth.provider.token(auth.repo)


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

    def __init__(self, repo: GitRepo, dir_: str, url: str = "", auth: Auth | None = None) -> None:
        self._repo = repo
        self._dir = dir_
        # The clean clone URL (no embedded credential) and auth are kept so :meth:`push`
        # can re-resolve a fresh token per op — GitHub App installation tokens are
        # short-lived (~1h), so a token captured at clone time may be stale by push.
        self._url = url
        self._auth = auth if auth is not None else Auth()

    @staticmethod
    def clone(url: str, dir: str, auth: Auth | None = None) -> Repo:
        """Clone url into dir (which must not already exist). Auth is chosen by the URL
        scheme: for an https URL a token from ``auth.provider`` is embedded as GitHub HTTP
        basic auth; an ssh URL (``git@…``/``ssh://…``) is left untouched so system ``git``
        authenticates it via ssh-agent / default keys. A non-empty ``auth.ssh_key`` pins
        ssh to that private key (via ``GIT_SSH_COMMAND``); it is ignored for https URLs. A
        plaintext ``http://`` remote with a provider is refused (token leak).

        ``clone_from(env=...)`` scopes the ssh environment to the clone subprocess only, so
        it is persisted onto the returned repo's Git instance — a later :meth:`push` over
        ssh then reuses the same key (mirroring the Go reference's per-repo auth).
        """
        auth = auth if auth is not None else Auth()
        token = _token_for(url, auth)
        clone_url = _auth_url(url, token)
        env = _ssh_env(auth.ssh_key) if _is_ssh_url(url) else None
        try:
            repo = GitRepo.clone_from(clone_url, dir, env=env)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"clone {url}: {exc}") from exc
        if token:
            # Don't persist the credential: clone_from records the tokened URL as origin in
            # .git/config. Reset it to the clean URL; push() re-applies a fresh token only for
            # the network op and strips it again, so the token never lingers on disk (matching
            # the Go reference, which uses transport auth rather than an in-URL credential).
            try:
                repo.remote("origin").set_url(url)
            except Exception as exc:  # noqa: BLE001
                raise ValueError(f"clone {url}: reset remote url: {exc}") from exc
        if env:
            # GitPython does NOT carry the clone env onto the returned Repo; set it
            # explicitly so push() (and any later ssh op) keeps using GIT_SSH_COMMAND.
            repo.git.update_environment(**env)
        return Repo(repo, dir, url, auth)

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
        """Push the current branch to origin. An up-to-date push is not an error.
        Credentials are re-resolved here (not reused from clone) so a fresh, repo-scoped
        token authenticates the push even if the clone-time token has since expired — for
        an https remote the origin URL is re-pointed at the freshly-tokened form; an ssh
        remote carries no in-URL credential (its auth is the persisted GIT_SSH_COMMAND). The
        tokened URL is applied only for the push and stripped again in a ``finally`` so the
        credential never lingers in .git/config (the clone-time origin URL is already clean)."""
        token = _token_for(self._url, self._auth)
        try:
            origin = self._repo.remote("origin")
            branch = self._repo.active_branch.name
            if token:
                origin.set_url(_auth_url(self._url, token))
            results = origin.push(refspec=f"{branch}:{branch}")
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"push: {exc}") from exc
        finally:
            if token:
                # Best effort: the temp checkout is removed after the attempt anyway.
                with contextlib.suppress(Exception):
                    self._repo.remote("origin").set_url(self._url)
        for info in results:
            if info.flags & info.ERROR:
                raise ValueError(f"push: {info.summary}")

    def head(self) -> str:
        """Return the current HEAD commit SHA."""
        try:
            return self._repo.head.commit.hexsha
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"head: {exc}") from exc
