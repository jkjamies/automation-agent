"""Tests for the webhook server using FastAPI's TestClient."""

from __future__ import annotations

import hashlib
import hmac

from fastapi.testclient import TestClient

from automation_agent.ingest import Envelope, Kind
from automation_agent.webhook import Server, verify_signature


class Capture:
    """Records the last Envelope; optionally raises to force a 500."""

    def __init__(self, err: Exception | None = None) -> None:
        self.env: Envelope | None = None
        self.err = err

    async def ingest(self, e: Envelope) -> None:
        self.env = e
        if self.err is not None:
            raise self.err


def sign(secret: str, body: str) -> str:
    mac = hmac.new(secret.encode(), body.encode(), hashlib.sha256)
    return "sha256=" + mac.hexdigest()


def test_lint_kickoff() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest).app)
    resp = client.post("/webhooks/lint", content='{"problems":[]}')

    assert resp.status_code == 202
    assert c.env is not None
    assert c.env.kind == Kind.LINT
    assert c.env.source == "webhook:/lint"
    assert c.env.payload == b'{"problems":[]}'


def test_coverage_kickoff() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest).app)
    resp = client.post("/webhooks/coverage", content='{"report":"jacoco"}')

    assert resp.status_code == 202
    assert c.env is not None
    assert c.env.kind == Kind.COVERAGE
    assert c.env.source == "webhook:/coverage"


def test_lint_kickoff_requires_signature() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest, secret="topsecret").app)
    body = '{"problems":[]}'
    # No signature -> rejected (kickoff selects a caller-supplied target repo).
    assert client.post("/webhooks/lint", content=body).status_code == 401
    assert c.env is None
    # Valid signature -> accepted.
    resp = client.post(
        "/webhooks/lint",
        content=body,
        headers={"X-Hub-Signature-256": sign("topsecret", body)},
    )
    assert resp.status_code == 202
    assert c.env is not None and c.env.kind == Kind.LINT


def test_coverage_kickoff_requires_signature() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest, secret="topsecret").app)
    resp = client.post("/webhooks/coverage", content='{"report":"jacoco"}')
    assert resp.status_code == 401
    assert c.env is None


def test_github_signature_valid() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest, secret="topsecret").app)
    body = '{"action":"completed"}'
    resp = client.post(
        "/webhooks/github",
        content=body,
        headers={"X-Hub-Signature-256": sign("topsecret", body)},
    )

    assert resp.status_code == 202
    assert c.env is not None
    assert c.env.kind == Kind.CI


def test_github_signature_invalid() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest, secret="topsecret").app)
    resp = client.post(
        "/webhooks/github",
        content="{}",
        headers={"X-Hub-Signature-256": "sha256=deadbeef"},
    )
    assert resp.status_code == 401


def test_github_missing_signature() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest, secret="topsecret").app)
    resp = client.post("/webhooks/github", content="{}")
    assert resp.status_code == 401


def test_github_no_secret_skips_verification() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest).app)  # no secret
    resp = client.post("/webhooks/github", content="{}")
    assert resp.status_code == 202


def test_ingest_error_is_500() -> None:
    c = Capture(err=RuntimeError("boom"))
    client = TestClient(Server(c.ingest).app)
    resp = client.post("/webhooks/lint", content="{}")
    assert resp.status_code == 500


def test_healthz() -> None:
    client = TestClient(Server(Capture().ingest).app)
    resp = client.get("/healthz")
    assert resp.status_code == 200
    assert resp.text == "ok"


def test_method_not_allowed() -> None:
    client = TestClient(Server(Capture().ingest).app)
    resp = client.get("/webhooks/lint")
    assert resp.status_code == 405


def test_oversize_body_is_rejected() -> None:
    # A body larger than the cap is rejected with 413 (not truncated): a truncated
    # body would fail HMAC and could feed malformed JSON downstream.
    c = Capture()
    client = TestClient(Server(c.ingest).app)
    oversize = "x" * ((5 << 20) + 100)
    resp = client.post("/webhooks/lint", content=oversize)
    assert resp.status_code == 413
    assert c.env is None  # never dispatched


def test_at_cap_body_is_accepted() -> None:
    # A body exactly at the cap is accepted in full.
    c = Capture()
    client = TestClient(Server(c.ingest).app)
    body = "x" * (5 << 20)
    resp = client.post("/webhooks/lint", content=body)
    assert resp.status_code == 202
    assert c.env is not None
    assert len(c.env.payload) == (5 << 20)


def test_verify_signature_directly() -> None:
    body = b'{"action":"completed"}'
    good = sign("topsecret", body.decode())
    assert verify_signature("topsecret", good, body)
    assert not verify_signature("topsecret", "sha256=deadbeef", body)
    assert not verify_signature("topsecret", "", body)
    assert not verify_signature("topsecret", "deadbeef", body)  # missing prefix


# --- /internal/* (Cloud Scheduler ingress) -----------------------------------


def _bearer(token: str) -> dict[str, str]:
    return {"Authorization": f"Bearer {token}"}


def test_internal_endpoints_disabled_without_token() -> None:
    # No internal_token -> the endpoints are off (404), never open by default.
    c = Capture()
    client = TestClient(Server(c.ingest).app)
    for path in ("/internal/cron/daily", "/internal/sweep"):
        assert client.post(path).status_code == 404
    assert c.env is None


def test_internal_cron_requires_bearer_token() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest, internal_token="s3cret").app)
    # Missing / wrong token -> 401.
    assert client.post("/internal/cron/daily").status_code == 401
    assert client.post("/internal/cron/daily", headers=_bearer("nope")).status_code == 401
    assert c.env is None


def test_internal_cron_daily_dispatches() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest, internal_token="s3cret").app)
    resp = client.post("/internal/cron/daily", headers=_bearer("s3cret"))
    assert resp.status_code == 202
    assert c.env is not None
    assert c.env.kind == Kind.CRON_DAILY
    assert c.env.source == "internal:/cron/daily"


def test_internal_sweep_runs_sweep_func() -> None:
    ran = {"n": 0}

    async def _sweep() -> None:
        ran["n"] += 1

    client = TestClient(Server(Capture().ingest, internal_token="s3cret", sweep=_sweep).app)
    resp = client.post("/internal/sweep", headers=_bearer("s3cret"))
    assert resp.status_code == 200
    assert ran["n"] == 1


def test_internal_sweep_not_configured_is_501() -> None:
    # internal_token set but no sweep func wired -> 501 (auth still required).
    client = TestClient(Server(Capture().ingest, internal_token="s3cret").app)
    assert client.post("/internal/sweep").status_code == 401  # auth first
    assert client.post("/internal/sweep", headers=_bearer("s3cret")).status_code == 501


def test_internal_sweep_error_is_500() -> None:
    async def _boom() -> None:
        raise RuntimeError("sweep boom")

    client = TestClient(Server(Capture().ingest, internal_token="s3cret", sweep=_boom).app)
    resp = client.post("/internal/sweep", headers=_bearer("s3cret"))
    assert resp.status_code == 500
