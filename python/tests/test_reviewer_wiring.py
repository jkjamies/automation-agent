"""Wiring tests for the reviewer's cross-cutting seams: the ingest KindReview codec, the
/webhooks/github event routing (pull_request → review, check_run → ci, else 200 no-dispatch), and
the REVIEW_* config parsing/validation."""

from __future__ import annotations

import base64

import pytest
from fastapi.testclient import TestClient

from automation_agent.config import load_from
from automation_agent.ingest import Envelope, Kind, decode, encode, new
from automation_agent.webhook import Server


class Capture:
    def __init__(self) -> None:
        self.env: Envelope | None = None

    async def ingest(self, e: Envelope) -> None:
        self.env = e


# --- ingest KindReview -------------------------------------------------------


def test_kind_review_is_valid_and_round_trips() -> None:
    assert Kind.REVIEW.value == "review" and Kind.REVIEW.valid()
    from datetime import UTC, datetime

    e = new(Kind.REVIEW, "webhook:/github", b"payload", datetime.now(UTC))
    wire = encode(e)
    parsed = decode(wire)
    assert parsed.kind is Kind.REVIEW and parsed.payload == b"payload"
    # payload is a base64 string field in the wire form
    import json

    assert json.loads(wire)["payload"] == base64.standard_b64encode(b"payload").decode()


# --- /webhooks/github routing ------------------------------------------------


def test_github_pull_request_routes_to_review() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest).app)
    resp = client.post("/webhooks/github", content="{}", headers={"X-GitHub-Event": "pull_request"})
    assert resp.status_code == 202
    assert c.env is not None and c.env.kind is Kind.REVIEW and c.env.source == "webhook:/github"


def test_github_check_run_routes_to_ci() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest).app)
    resp = client.post("/webhooks/github", content="{}", headers={"X-GitHub-Event": "check_run"})
    assert resp.status_code == 202
    assert c.env is not None and c.env.kind is Kind.CI


def test_github_other_event_acks_without_dispatch() -> None:
    c = Capture()
    client = TestClient(Server(c.ingest).app)
    resp = client.post("/webhooks/github", content="{}", headers={"X-GitHub-Event": "ping"})
    assert resp.status_code == 200 and c.env is None
    # No header at all is also a no-dispatch 200.
    resp = client.post("/webhooks/github", content="{}")
    assert resp.status_code == 200 and c.env is None


# --- config REVIEW_* ---------------------------------------------------------


def test_review_config_defaults() -> None:
    cfg = load_from(lambda k: None)
    assert cfg.review_enabled is False and cfg.review_skip_drafts is True
    assert cfg.review_max_files == 50 and cfg.review_max_diff_bytes == 256 * 1024
    assert cfg.review_min_confidence == 0.6 and cfg.review_uncited_mode == "nitpick"
    assert cfg.review_standards is True and cfg.review_standards_max_bytes == 256 * 1024
    assert "go.sum" in cfg.review_exclude_globs and "AGENTS.md" in cfg.review_standards_globs
    from datetime import timedelta

    assert cfg.review_debounce == timedelta(seconds=30)


def test_review_config_overrides_and_validation() -> None:
    env = {
        "REVIEW_ENABLED": "true",
        "REVIEW_MAX_FILES": "10",
        "REVIEW_MIN_CONFIDENCE": "0.4",
        "REVIEW_UNCITED_MODE": "drop",
        "REVIEW_DEBOUNCE": "45s",
        "REVIEW_EXCLUDE_GLOBS": "*.min.js,foo/**",
    }
    cfg = load_from(env.get)
    assert cfg.review_enabled and cfg.review_max_files == 10 and cfg.review_min_confidence == 0.4
    assert cfg.review_uncited_mode == "drop"
    assert cfg.review_exclude_globs == ["*.min.js", "foo/**"]

    for bad in (
        {"REVIEW_STANDARDS_MAX_BYTES": "0"},
        {"REVIEW_MIN_CONFIDENCE": "1.5"},
        {"REVIEW_UNCITED_MODE": "bogus"},
    ):
        with pytest.raises(ValueError):
            load_from(bad.get)
