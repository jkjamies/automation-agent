"""Tests for the ParkStore: the durable replacement for the old in-memory registry.

Covers the atomic single-winner claim (resolve_by_pr_key / sweep), the re-park index
hygiene, the empty-key guard, and terminal delete. The in-memory backend runs in one
asyncio event loop, so "atomicity" is the absence of preemption between the index lookup
and the claim — a second resolve always finds nothing.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

from automation_agent.agent.setup import MemoryParkStore, ParkRecord, new_park_store
from automation_agent.config import SessionBackend, load_from


def _now() -> datetime:
    return datetime.now(UTC)


async def test_put_get_roundtrip() -> None:
    store = MemoryParkStore()
    await store.put(ParkRecord(session_id="s1", params='{"k":1}'))
    got = await store.get("s1")
    assert got is not None and got.params == '{"k":1}'
    assert got.pr_key == "" and not got.parked
    assert await store.get("missing") is None


async def test_get_returns_a_copy() -> None:
    # Mutating a returned record must not corrupt stored state (value semantics).
    store = MemoryParkStore()
    await store.put(ParkRecord(session_id="s1", params="orig"))
    got = await store.get("s1")
    assert got is not None
    got.params = "mutated"
    again = await store.get("s1")
    assert again is not None and again.params == "orig"


async def test_resolve_claims_once() -> None:
    store = MemoryParkStore()
    await store.put(
        ParkRecord(session_id="s1", pr_key="o/r#1", call_id="c", attempts=2, parked_at=_now())
    )
    assert await store.parked_count() == 1

    run = await store.resolve_by_pr_key("o/r#1")
    assert run is not None and run.call_id == "c" and run.attempts == 2
    assert run.pr_key == ""  # claim clears the index field on the returned record
    # The per-run record is retained (for a retry) but no longer parked.
    assert await store.resolve_by_pr_key("o/r#1") is None  # already claimed
    assert await store.parked_count() == 0
    assert await store.get("s1") is not None  # record retained until delete


async def test_resolve_empty_and_unknown_key() -> None:
    store = MemoryParkStore()
    assert await store.resolve_by_pr_key("") is None
    assert await store.resolve_by_pr_key("never/parked#9") is None


async def test_repark_drops_stale_index() -> None:
    # Re-parking the same session under a new PR key must not leave the old key resolvable.
    store = MemoryParkStore()
    await store.put(
        ParkRecord(session_id="s1", pr_key="o/r#1", call_id="c1", attempts=1, parked_at=_now())
    )
    await store.put(
        ParkRecord(session_id="s1", pr_key="o/r#2", call_id="c2", attempts=2, parked_at=_now())
    )
    assert await store.parked_count() == 1
    assert await store.resolve_by_pr_key("o/r#1") is None  # stale index gone
    run = await store.resolve_by_pr_key("o/r#2")
    assert run is not None and run.call_id == "c2" and run.attempts == 2


async def test_sweep_claims_stale_only() -> None:
    store = MemoryParkStore()
    old = _now() - timedelta(hours=2)
    fresh = _now()
    await store.put(
        ParkRecord(session_id="old", pr_key="o/r#1", call_id="c", attempts=1, parked_at=old)
    )
    await store.put(
        ParkRecord(session_id="new", pr_key="o/r#2", call_id="c", attempts=1, parked_at=fresh)
    )

    swept = await store.sweep(_now() - timedelta(hours=1))
    assert [r.pr_key for r in swept] == ["o/r#1"]  # only the stale one, pr_key restored
    assert swept[0].session_id == "old"
    # The swept run is claimed (no longer parked); the fresh one remains parked.
    assert await store.resolve_by_pr_key("o/r#1") is None
    assert await store.parked_count() == 1
    assert await store.resolve_by_pr_key("o/r#2") is not None


async def test_delete_removes_record_and_index() -> None:
    store = MemoryParkStore()
    await store.put(ParkRecord(session_id="s1", pr_key="o/r#1", call_id="c", parked_at=_now()))
    await store.delete("s1")
    assert await store.get("s1") is None
    assert await store.resolve_by_pr_key("o/r#1") is None
    assert await store.parked_count() == 0
    await store.delete("missing")  # no-op


def test_new_park_store_memory() -> None:
    cfg = load_from({"SESSION_BACKEND": "memory"}.get)
    assert isinstance(new_park_store(cfg), MemoryParkStore)


def test_new_park_store_unimplemented() -> None:
    cfg = load_from({"SESSION_BACKEND": "sqlite"}.get)
    assert cfg.session_backend == SessionBackend.SQLITE
    try:
        new_park_store(cfg)
    except NotImplementedError:
        pass
    else:  # pragma: no cover - guards against silently shipping a stub
        raise AssertionError("expected NotImplementedError for sqlite backend")
