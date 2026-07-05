"""Tests for the auth seam (TokenProvider).

StaticProvider needs no network. AppProvider is exercised against a localhost stub of
the GitHub installation token-exchange endpoint: a throwaway RSA key signs the App JWT,
and the stub captures it to assert RS256 / issuer / the pinned-installation path, plus
caching and refresh. No live network, no LLM.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import jwt
import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

from automation_agent.auth import AppProvider, StaticProvider, new_app_provider


def _rsa_pem() -> str:
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    return key.private_bytes(
        encoding=serialization.Encoding.PEM,
        format=serialization.PrivateFormat.PKCS8,
        encryption_algorithm=serialization.NoEncryption(),
    ).decode()


_PEM = _rsa_pem()
_FAR_FUTURE = "2099-01-01T00:00:00Z"
_PAST = "2000-01-01T00:00:00Z"


class _Stub:
    """A localhost stub of POST /app/installations/{id}/access_tokens. Captures each
    request's path + Authorization (the App JWT) and returns a fixed installation token
    with a configurable ``expires_at`` (mutate before a call to drive cache vs refresh)."""

    def __init__(self) -> None:
        self.expires_at = _FAR_FUTURE
        self.requests: list[tuple[str, str]] = []
        self._server = HTTPServer(("127.0.0.1", 0), self._handler())
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address
        return f"http://{host}:{port}"

    @property
    def token_requests(self) -> list[tuple[str, str]]:
        return [r for r in self.requests if "access_tokens" in r[0]]

    @property
    def app_requests(self) -> list[tuple[str, str]]:
        return [r for r in self.requests if r[0].endswith("/app")]

    def start(self) -> None:
        self._thread.start()

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()

    def _handler(stub):  # noqa: N805 — closure over the stub instance
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args) -> None:  # silence test output
                pass

            def do_POST(self) -> None:  # noqa: N802 (BaseHTTPRequestHandler API)
                length = int(self.headers.get("Content-Length", 0))
                self.rfile.read(length)
                stub.requests.append((self.path, self.headers.get("Authorization", "")))
                body = json.dumps(
                    {"token": "ghs_installation_token", "expires_at": stub.expires_at}
                ).encode()
                self.send_response(201)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self) -> None:  # noqa: N802 (BaseHTTPRequestHandler API)
                # GET /app resolves the App's own identity (its slug) for authored-login.
                stub.requests.append((self.path, self.headers.get("Authorization", "")))
                body = json.dumps({"id": 42, "slug": "automation-agent"}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        return Handler


@pytest.fixture
def stub():
    s = _Stub()
    s.start()
    yield s
    s.stop()


def test_static_provider_returns_constant_token() -> None:
    p = StaticProvider("pat-123")
    assert p.token("acme/api") == "pat-123"
    assert p.token("other/repo") == "pat-123"  # repo ignored
    assert p.github() is p.github()  # one cached client


def test_static_provider_empty_is_anonymous() -> None:
    p = StaticProvider("")
    assert p.token("acme/api") == ""
    assert p.github() is not None  # unauthenticated client, no crash


def test_app_provider_mints_token_pinned_installation(stub) -> None:
    p = new_app_provider(42, 99, _PEM, base_url=stub.base_url)
    assert isinstance(p, AppProvider)

    tok = p.token("acme/api")
    assert tok == "ghs_installation_token"

    assert len(stub.token_requests) == 1
    path, authorization = stub.token_requests[0]
    # Pinned single installation (Decision §1): the exchange targets installation 99.
    assert path.endswith("/app/installations/99/access_tokens")
    # The request authenticates as the App with an RS256 JWT issued by the app id.
    assert authorization.startswith("Bearer ")
    app_jwt = authorization.removeprefix("Bearer ")
    assert jwt.get_unverified_header(app_jwt)["alg"] == "RS256"
    claims = jwt.decode(app_jwt, options={"verify_signature": False})
    assert claims["iss"] == "42"


def test_app_provider_caches_token(stub) -> None:
    stub.expires_at = _FAR_FUTURE
    p = new_app_provider(42, 99, _PEM, base_url=stub.base_url)
    assert p.token("acme/api") == "ghs_installation_token"
    assert p.token("acme/api") == "ghs_installation_token"
    # A still-valid token is reused: only one exchange.
    assert len(stub.token_requests) == 1


def test_app_provider_refreshes_expired_token(stub) -> None:
    stub.expires_at = _PAST  # already expired → each read re-exchanges
    p = new_app_provider(42, 99, _PEM, base_url=stub.base_url)
    p.token("acme/api")
    p.token("acme/api")
    assert len(stub.token_requests) == 2


def test_app_provider_shares_one_client(stub) -> None:
    p = new_app_provider(42, 99, _PEM, base_url=stub.base_url)
    assert p.github() is p.github()


def test_new_app_provider_accepts_pem_bytes(stub) -> None:
    p = new_app_provider(42, 99, _PEM.encode("utf-8"), base_url=stub.base_url)
    assert p.token("acme/api") == "ghs_installation_token"


def test_static_provider_empty_authored_login_is_anonymous() -> None:
    # No token → no identity to attribute, and no network call.
    assert StaticProvider("").authored_login() == ""


def test_static_provider_authored_login_resolves_pat_user(monkeypatch) -> None:
    # A non-empty token resolves the authenticated user login via GET /user.
    p = StaticProvider("pat-123")

    class _User:
        login = "octocat"

    monkeypatch.setattr(p.github(), "get_user", lambda: _User())
    assert p.authored_login() == "octocat"


def test_app_provider_authored_login_resolves_bot_slug(stub) -> None:
    p = new_app_provider(42, 99, _PEM, base_url=stub.base_url)
    assert p.authored_login() == "automation-agent[bot]"
    # The App authenticates the GET /app with an RS256 JWT issued by the app id.
    assert stub.app_requests
    _, authorization = stub.app_requests[0]
    app_jwt = authorization.removeprefix("Bearer ")
    assert jwt.get_unverified_header(app_jwt)["alg"] == "RS256"
    # Resolved once and cached: a second call makes no further request.
    p.authored_login()
    assert len(stub.app_requests) == 1
