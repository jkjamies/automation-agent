"""Tests for the fixflow registry: atomic single-winner resolve, re-park, and the
asyncio timeout firing.

The registry runs in a single asyncio event loop, so "atomicity" is the absence of preemption between
the dict pop and the timer cancel — a second resolve in the same loop always finds
nothing. The timeout is tested with a tiny ci_timeout and an ``await asyncio.sleep`` to
let the loop's ``call_later`` fire.
"""

from __future__ import annotations

import asyncio
from datetime import timedelta

from automation_agent.agent.fixflow.registry import ParkedRun, RunRegistry


async def _no_timeout(_pr: str) -> None:
    return None


async def test_registry_resolve_once() -> None:
    r = RunRegistry()
    r.park("o/r#1", ParkedRun(session_id="s", call_id="c"), timedelta(hours=1), _no_timeout)
    assert r.len() == 1

    run = r.resolve("o/r#1")
    assert run is not None and run.call_id == "c"
    assert r.resolve("o/r#1") is None  # already claimed
    assert r.len() == 0


async def test_registry_late_resolve_noop() -> None:
    r = RunRegistry()
    assert r.resolve("never/parked#9") is None


async def test_registry_timeout_claims() -> None:
    r = RunRegistry()
    claimed: list[bool] = []

    async def on_timeout(pr: str) -> None:
        claimed.append(r.resolve(pr) is not None)

    r.park("o/r#2", ParkedRun(session_id="s", call_id="c"), timedelta(seconds=0.05), on_timeout)
    await asyncio.sleep(0.15)

    assert claimed == [True], "timeout fired and claimed the parked run"
    assert r.len() == 0
    # Late webhook after the timeout claimed it.
    assert r.resolve("o/r#2") is None


async def test_registry_resolve_before_timeout_cancels() -> None:
    r = RunRegistry()
    fired: list[str] = []

    async def on_timeout(pr: str) -> None:  # pragma: no cover - must not fire
        fired.append(pr)

    r.park("o/r#5", ParkedRun(session_id="s", call_id="c"), timedelta(seconds=0.05), on_timeout)
    assert r.resolve("o/r#5") is not None
    await asyncio.sleep(0.15)
    assert fired == [], "timer should have been cancelled by resolve"


async def test_registry_repark() -> None:
    r = RunRegistry()
    r.park("o/r#4", ParkedRun(session_id="s", call_id="c1", attempts=1), timedelta(hours=1), _no_timeout)
    r.park("o/r#4", ParkedRun(session_id="s", call_id="c2", attempts=2), timedelta(hours=1), _no_timeout)
    assert r.len() == 1
    run = r.resolve("o/r#4")
    assert run is not None and run.call_id == "c2" and run.attempts == 2
