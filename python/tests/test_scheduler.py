"""Tests for the scheduler (no real-time cron waits)."""

from __future__ import annotations

from datetime import UTC, datetime

import pytest

from automation_agent.ingest import Envelope, Kind
from automation_agent.scheduler import Scheduler


def test_add_valid_and_invalid() -> None:
    s = Scheduler(lambda e: None)
    s.add("0 9 * * *", Kind.CRON_DAILY)
    s.add("0 9 * * 1", Kind.CRON_WEEKLY)
    assert s.entries() == 2
    with pytest.raises(ValueError):
        s.add("not a cron spec", Kind.CRON_DAILY)


def test_trigger_emits_envelope() -> None:
    captured: list[Envelope] = []
    s = Scheduler(captured.append)
    fixed = datetime.fromtimestamp(1718870400, tz=UTC)
    s.now = lambda: fixed

    s._trigger(Kind.CRON_WEEKLY)

    assert len(captured) == 1
    got = captured[0]
    assert got.kind == Kind.CRON_WEEKLY
    assert got.source == "scheduler"
    assert got.payload == b""
    assert got.received_at == fixed
