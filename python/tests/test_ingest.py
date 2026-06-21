"""Port of internal/ingest/envelope_test.py."""

from __future__ import annotations

from datetime import UTC, datetime

from automation_agent.ingest import Kind, new


def test_kind_valid() -> None:
    for k in (Kind.CRON_DAILY, Kind.CRON_WEEKLY, Kind.LINT, Kind.COVERAGE, Kind.CI):
        assert k.valid()


def test_new() -> None:
    at = datetime.fromtimestamp(1718870400, tz=UTC)
    e = new(Kind.LINT, "webhook:/lint", b'{"x":1}', at)
    assert e.kind == Kind.LINT
    assert e.source == "webhook:/lint"
    assert e.payload == b'{"x":1}'
    assert e.received_at == at
