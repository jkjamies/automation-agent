"""Tests for the ParkStore: the durable replacement for the old in-memory registry.

The conformance tests run against EVERY backend (memory, sqlite, and — when the Firestore
emulator is available — firestore) via the ``store`` fixture, covering the atomic
single-winner claim (resolve_by_pr_key / sweep), the re-park index hygiene, the empty-key
guard, and terminal delete. A separate test exercises sqlite durability across a simulated
restart. The firestore parameter is skipped unless ``FIRESTORE_EMULATOR_HOST`` is set.
"""

from __future__ import annotations

import os
import uuid
from collections.abc import AsyncIterator
from datetime import UTC, datetime, timedelta

import pytest

from automation_agent.agent.setup import (
    MemoryParkStore,
    ParkRecord,
    SqliteParkStore,
    new_park_store,
)
from automation_agent.config import SessionBackend, load_from

_HAS_EMULATOR = bool(os.environ.get("FIRESTORE_EMULATOR_HOST"))
_needs_emulator = pytest.mark.skipif(
    not _HAS_EMULATOR, reason="needs the Firestore emulator (FIRESTORE_EMULATOR_HOST)"
)


def _now() -> datetime:
    return datetime.now(UTC)


@pytest.fixture(
    params=[
        "memory",
        "sqlite",
        pytest.param("firestore", marks=_needs_emulator),
    ]
)
async def store(request, tmp_path) -> AsyncIterator:
    """A ParkStore of each backend, so the conformance tests run against all of them."""
    if request.param == "memory":
        yield MemoryParkStore()
        return
    if request.param == "sqlite":
        s = SqliteParkStore(str(tmp_path / "park.db"))
        try:
            yield s
        finally:
            await s.close()
        return
    # firestore (emulator): a unique collection per test isolates concurrent runs.
    from automation_agent.agent.setup.parkstore_firestore import FirestoreParkStore

    fs = FirestoreParkStore("test-project", "park_" + uuid.uuid4().hex)
    try:
        yield fs
    finally:
        await fs.close()


async def test_put_get_roundtrip(store) -> None:
    await store.put(ParkRecord(session_id="s1", params='{"k":1}'))
    got = await store.get("s1")
    assert got is not None and got.params == '{"k":1}'
    assert got.pr_key == "" and not got.parked
    assert await store.get("missing") is None


async def test_get_returns_a_copy(store) -> None:
    # Mutating a returned record must not corrupt stored state (value semantics).
    await store.put(ParkRecord(session_id="s1", params="orig"))
    got = await store.get("s1")
    assert got is not None
    got.params = "mutated"
    again = await store.get("s1")
    assert again is not None and again.params == "orig"


async def test_resolve_claims_once(store) -> None:
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


async def test_resolve_empty_and_unknown_key(store) -> None:
    assert await store.resolve_by_pr_key("") is None
    assert await store.resolve_by_pr_key("never/parked#9") is None


async def test_repark_drops_stale_index(store) -> None:
    # Re-parking the same session under a new PR key must not leave the old key resolvable.
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


async def test_sweep_claims_stale_only(store) -> None:
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


async def test_delete_removes_record_and_index(store) -> None:
    await store.put(ParkRecord(session_id="s1", pr_key="o/r#1", call_id="c", parked_at=_now()))
    await store.delete("s1")
    assert await store.get("s1") is None
    assert await store.resolve_by_pr_key("o/r#1") is None
    assert await store.parked_count() == 0
    await store.delete("missing")  # no-op


async def test_sqlite_survives_restart(tmp_path) -> None:
    # A parked run persists across a process restart (new store on the same file).
    path = str(tmp_path / "park.db")
    s1 = SqliteParkStore(path)
    await s1.put(
        ParkRecord(
            session_id="run-1",
            pr_key="o/r#7",
            call_id="c",
            attempts=2,
            params='{"owner":"o"}',
            parked_at=_now(),
        )
    )
    await s1.close()

    s2 = SqliteParkStore(path)  # "restart": fresh store, same file
    try:
        assert await s2.parked_count() == 1
        run = await s2.resolve_by_pr_key("o/r#7")
        assert run is not None and run.attempts == 2 and run.params == '{"owner":"o"}'
    finally:
        await s2.close()


def test_new_park_store_memory() -> None:
    cfg = load_from({"SESSION_BACKEND": "memory"}.get)
    assert isinstance(new_park_store(cfg), MemoryParkStore)


def test_new_park_store_sqlite(tmp_path) -> None:
    # The store opens its connection lazily (on first use), so this only checks wiring; a
    # tmp_path DSN keeps it hermetic in case that ever changes.
    cfg = load_from({"SESSION_BACKEND": "sqlite", "SQLITE_DSN": str(tmp_path / "park.db")}.get)
    store = new_park_store(cfg)
    assert isinstance(store, SqliteParkStore)


@_needs_emulator
def test_new_park_store_firestore() -> None:
    from automation_agent.agent.setup.parkstore_firestore import FirestoreParkStore

    cfg = load_from({"SESSION_BACKEND": "firestore", "FIRESTORE_PROJECT": "test-project"}.get)
    assert cfg.session_backend == SessionBackend.FIRESTORE
    store = new_park_store(cfg)
    assert isinstance(store, FirestoreParkStore)
