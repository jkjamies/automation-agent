"""Durable store of suspended fix runs — the fix-loop spine.

A :class:`ParkStore` persists one suspended fix run's state so a resume — or, with a
durable backend, a process restart — can continue it. It replaces the old in-memory
``RunRegistry``: the registry bundled the record, the PR index, and the asyncio timer
together and lived only in memory; the store holds just the durable record (keyed by
session id, indexed by PR key), while the soft per-run timeout timer now lives on the
:class:`~automation_agent.agent.fixflow.driver.Driver` (in-process, lost on restart) with
:meth:`ParkStore.sweep` as the durable catch-all.

The store interface is async so the same shape backs the in-memory, sqlite, and firestore
implementations (the latter two land in later phases). ``params`` is an opaque,
caller-serialized blob the store never interprets, which keeps it free of caller-specific
(fixflow) types.

Implementations MUST make :meth:`resolve_by_pr_key` (and :meth:`sweep`) an atomic claim:
for one PR key exactly one concurrent caller gets the record and all others get ``None``.
That single-winner guarantee is what makes a late or duplicate CI webhook — or a timeout
racing a webhook — safe: the loser finds nothing and no-ops.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass, replace
from datetime import datetime
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from automation_agent.config import Config


@dataclass
class ParkRecord:
    """One suspended fix run's stored state.

    Keyed by ``session_id`` (stable from kickoff). Once the run parks awaiting CI it is
    also indexed by ``pr_key``, which is how a CI webhook — which knows only the PR, not
    our session id — finds the run to resume.
    """

    session_id: str
    pr_key: str = ""  # empty until the run parks; the resume index
    call_id: str = ""  # the parked long-running call id
    attempts: int = 0  # attempts made so far (counted by the caller, not GitHub)
    params: str = ""  # opaque, caller-serialized run inputs (JSON)
    parked_at: datetime | None = None  # None until parked; the sweep cutoff field

    @property
    def parked(self) -> bool:
        """Whether the record is currently parked awaiting CI."""
        return self.pr_key != ""


class ParkStore(ABC):
    """Persists suspended fix runs. See the module docstring for the atomic-claim contract.

    A record has two distinct lifetimes: the per-run record (keyed by session id) lives for
    the whole multi-attempt run, while the PR-key index is per-park — claimed by
    :meth:`resolve_by_pr_key` and re-established on each re-park.
    """

    @abstractmethod
    async def put(self, record: ParkRecord) -> None:
        """Create or replace the per-run record keyed by ``record.session_id``,
        (re)establishing the PR-key index when ``record.pr_key`` is non-empty."""

    @abstractmethod
    async def get(self, session_id: str) -> ParkRecord | None:
        """Return the per-run record for ``session_id`` (``None`` if absent)."""

    @abstractmethod
    async def resolve_by_pr_key(self, pr_key: str) -> ParkRecord | None:
        """Atomically claim the parked record for ``pr_key``: clear the PR-key index (so a
        later duplicate no-ops) and return the record. The per-run record is retained so a
        retry can still read its params — terminal cleanup is :meth:`delete`. ``None`` for
        late/duplicate/unknown callers."""

    @abstractmethod
    async def delete(self, session_id: str) -> None:
        """Remove the per-run record (and any lingering index) for ``session_id``. Terminal
        cleanup; no-op if absent."""

    @abstractmethod
    async def sweep(self, cutoff: datetime) -> list[ParkRecord]:
        """Atomically claim and return every parked record whose ``parked_at`` is before
        ``cutoff`` (CI never reported). Like :meth:`resolve_by_pr_key`, each record is
        claimed once. The returned records keep their ``pr_key`` so the caller knows which
        PR timed out."""

    @abstractmethod
    async def parked_count(self) -> int:
        """How many records are currently parked (PR-key-indexed)."""


class MemoryParkStore(ParkStore):
    """Keeps park records in memory: the default backend, used by tests and ephemeral local
    runs (a restart strands parked runs). ``_by_session`` holds the per-run records;
    ``_index`` maps an active PR key to its session id.

    The whole driver runs in one asyncio event loop, so there is no preemption between the
    index lookup and the claim — :meth:`resolve_by_pr_key`/:meth:`sweep` are naturally
    atomic without a lock. Records are copied in and out so a caller mutating a returned
    record cannot corrupt stored state (value semantics, mirroring the Go reference).
    """

    def __init__(self) -> None:
        self._by_session: dict[str, ParkRecord] = {}
        self._index: dict[str, str] = {}  # pr_key -> session_id

    async def put(self, record: ParkRecord) -> None:
        # Drop a stale index entry if this session was previously parked under a different key.
        prev = self._by_session.get(record.session_id)
        if prev is not None and prev.pr_key != "" and prev.pr_key != record.pr_key:
            self._index.pop(prev.pr_key, None)
        self._by_session[record.session_id] = replace(record)
        if record.pr_key != "":
            self._index[record.pr_key] = record.session_id

    async def get(self, session_id: str) -> ParkRecord | None:
        rec = self._by_session.get(session_id)
        return replace(rec) if rec is not None else None

    async def resolve_by_pr_key(self, pr_key: str) -> ParkRecord | None:
        if pr_key == "":
            return None  # never resolve by an empty key (parity with sqlite)
        sid = self._index.get(pr_key)
        if sid is None:
            return None
        return self._claim(pr_key, sid)

    async def sweep(self, cutoff: datetime) -> list[ParkRecord]:
        out: list[ParkRecord] = []
        # Snapshot the index: _claim mutates it as we go.
        for pr_key, sid in list(self._index.items()):
            rec = self._by_session.get(sid)
            if rec is not None and rec.parked_at is not None and rec.parked_at < cutoff:
                claimed = self._claim(pr_key, sid)
                if claimed is not None:
                    claimed.pr_key = pr_key  # the timeout sweep needs which PR this was
                    out.append(claimed)
        return out

    async def delete(self, session_id: str) -> None:
        rec = self._by_session.pop(session_id, None)
        if rec is not None and rec.pr_key != "":
            self._index.pop(rec.pr_key, None)

    async def parked_count(self) -> int:
        return len(self._index)

    def _claim(self, pr_key: str, sid: str) -> ParkRecord | None:
        """Clear the PR-key index for ``sid`` and return a copy of the (now un-parked)
        record. The per-run record is retained for a possible retry."""
        self._index.pop(pr_key, None)
        rec = self._by_session.get(sid)
        if rec is None:
            return None
        cleared = replace(rec, pr_key="")  # un-parked; retained for a retry
        self._by_session[sid] = cleared
        return replace(cleared)  # a copy, so the caller can't corrupt stored state


def new_park_store(cfg: Config) -> ParkStore:
    """Build the park-record store for the configured backend, mirroring the session
    backend. sqlite and firestore land in later phases."""
    from automation_agent.config import SessionBackend

    if cfg.session_backend == SessionBackend.MEMORY:
        return MemoryParkStore()
    raise NotImplementedError(
        f"park store backend {cfg.session_backend!r} not yet implemented "
        "(sqlite/firestore land in a later phase); use SESSION_BACKEND=memory"
    )
