"""Package auth abstracts how the service authenticates to GitHub behind a single
seam, the :class:`TokenProvider`, so the rest of the code never sees whether a token
came from a static PAT or a freshly minted GitHub App installation token.

Two providers implement the seam:

  * :class:`StaticProvider` — returns one constant token for every repo. Backs the PAT
    local-dev fallback (``GITHUB_TOKEN`` / ``GH_TOKEN`` / ``gh auth token``) and the
    empty, anonymous client used for public reads and tests.
  * :class:`AppProvider` — mints and caches a short-lived (~1h), auto-refreshed
    installation token for a single pinned installation (single-org per deployment; see
    ``specs/20260625-github-app-authentication.md`` §1). The ``repo`` argument is
    accepted for the contract but ignored: one installation covers every repo in the
    deployment.

Each provider exposes two views of the same credential, one per consumer:

  * ``token(repo)`` — the raw token string the git transport embeds as
    ``x-access-token`` basic auth, re-fetched per git operation so a short-lived
    installation token stays current (mirrors the Go reference, where gitrepo calls
    ``Token`` per op).
  * ``github()`` — a ready PyGithub ``Github`` REST client. :class:`AppProvider` backs
    it with PyGithub's native ``AppInstallationAuth`` so the token auto-refreshes per
    request; the *same* underlying ``AppInstallationAuth`` backs ``token()``, so REST
    and git share one cached installation token (no double mint).

The Go reference bridges the seam with a token-injecting ``http.RoundTripper`` plus a
free ``Token`` call in gitrepo; PyGithub already owns the REST client and its auth
refresh, so the idiomatic Python shape is to let the provider hand back a ready client.
The external contract (env vars, mode selection, App-vs-PAT behavior) is identical.

Deterministic tooling — no agent imports.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from github import Auth, Github


@runtime_checkable
class TokenProvider(Protocol):
    """Yields a valid GitHub token / REST client for operations on a repo. PAT mode
    returns the same constant for every repo; App mode mints/caches an installation
    token and refreshes it before expiry. ``repo`` is ``"owner/name"``."""

    def token(self, repo: str) -> str:
        """Return a currently-valid token; ``""`` means anonymous (public read only)."""
        ...

    def github(self) -> Github:
        """Return the PyGithub REST client this provider authenticates."""
        ...


class StaticProvider:
    """Returns the same token for every repo. Backs PAT mode and the empty/anonymous
    client (an empty token yields an unauthenticated client, fine for public reads and
    tests)."""

    def __init__(self, token: str = "") -> None:
        self._token = token
        # An empty token must build an *unauthenticated* client (Auth.Token rejects "").
        self._gh = Github(auth=Auth.Token(token)) if token else Github()

    def token(self, repo: str) -> str:
        """Return the constant token; ``repo`` is ignored."""
        return self._token

    def github(self) -> Github:
        return self._gh


class AppProvider:
    """Mints and caches a short-lived installation token for a single pinned
    installation. PyGithub's ``AppInstallationAuth`` handles the JWT (RS256) minting, the
    token exchange, caching, and proactive refresh; :class:`AppProvider` adapts it to the
    seam and shares one ``AppInstallationAuth`` between the REST client and the
    git-token path so both reuse one cached installation token."""

    def __init__(
        self,
        app_id: int,
        installation_id: int,
        private_key_pem: str,
        *,
        base_url: str = "",
    ) -> None:
        app_auth = Auth.AppAuth(app_id, private_key_pem)
        self._inst = app_auth.get_installation_auth(installation_id)
        # Constructing the Github wires the requester onto _inst (so _inst.token can mint)
        # AND is the REST client itself: one client, native per-request token refresh.
        # base_url is overridden in tests to point at a local stub; production omits it
        # and uses the default https://api.github.com.
        if base_url:
            self._gh = Github(auth=self._inst, base_url=base_url)
        else:
            self._gh = Github(auth=self._inst)

    def token(self, repo: str) -> str:
        """Return a currently-valid installation token, minting on first call then
        refreshing before expiry. ``repo`` is ignored: the installation is pinned and
        covers every repo in the deployment."""
        return self._inst.token

    def github(self) -> Github:
        return self._gh


def new_app_provider(
    app_id: int,
    installation_id: int,
    private_key_pem: str | bytes,
    *,
    base_url: str = "",
) -> AppProvider:
    """Build an App provider pinned to one installation. ``private_key_pem`` is the App
    private key in PEM form — the caller sources and validates it (see ``config``). Bytes
    are decoded to text for PyGithub's ``AppAuth``."""
    pem = private_key_pem.decode("utf-8") if isinstance(private_key_pem, bytes) else private_key_pem
    return AppProvider(app_id, installation_id, pem, base_url=base_url)
