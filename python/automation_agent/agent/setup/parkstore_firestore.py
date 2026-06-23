"""Firestore-backed park store — the serverless, scale-to-zero cloud backend.

Mirrors the Go reference (``parkstore_firestore.go``) on the native async Firestore client.
The park record is *our* app concept (no ADK type), so unlike the session service — which
uses adk's native ``FirestoreSessionService`` — this store is hand-rolled on
``google.cloud.firestore.AsyncClient``.

The atomic claim runs in a Firestore transaction: of N concurrent resolvers, the first to
commit clears ``pr_key``; the others' transactions detect the change and retry, re-read the
now-cleared key, and find nothing — so exactly one wins. As with sqlite, the ``pr_key`` field
doubles as the resume index (``""`` when not parked), so re-parking under a new key cannot
leak a stale entry.

This module is import-light at module load (the firestore SDK is imported lazily) and is
exercised only against the Firestore emulator, so the default unit-coverage gate omits it
(see pyproject ``[tool.coverage.run] omit`` and the ``cover-firestore`` make target).
"""

from __future__ import annotations

from datetime import datetime
from typing import TYPE_CHECKING, Any

from automation_agent.agent.setup.parkstore import ParkRecord, ParkStore

if TYPE_CHECKING:
    from google.cloud.firestore_v1.async_transaction import AsyncTransaction
    from google.cloud.firestore_v1.base_document import DocumentSnapshot


def _doc_from_record(r: ParkRecord) -> dict[str, Any]:
    return {
        "session_id": r.session_id,
        "pr_key": r.pr_key,
        "call_id": r.call_id,
        "attempts": r.attempts,
        "params": r.params,
        "parked_at": r.parked_at,  # native firestore timestamp, or None
    }


def _record_from_doc(d: dict[str, Any]) -> ParkRecord:
    return ParkRecord(
        session_id=d.get("session_id", ""),
        pr_key=d.get("pr_key", ""),
        call_id=d.get("call_id", ""),
        attempts=d.get("attempts", 0),
        params=d.get("params", ""),
        parked_at=d.get("parked_at"),
    )


def _claim_snapshot(transaction: AsyncTransaction, snap: DocumentSnapshot) -> ParkRecord | None:
    """Clear a still-parked doc's pr_key inside a transaction and return the claimed record.
    A doc already cleared (pr_key=="") yields None so a racing claimer no-ops. All transaction
    reads must precede this call (it writes)."""
    d = snap.to_dict() or {}
    if d.get("pr_key", "") == "":
        return None
    transaction.update(snap.reference, {"pr_key": ""})
    d["pr_key"] = ""
    return _record_from_doc(d)


class FirestoreParkStore(ParkStore):
    """Persists park records to Firestore. ``close`` releases the client."""

    def __init__(self, project: str, collection: str) -> None:
        from google.cloud import firestore

        # project "" -> None: detect from ADC / GOOGLE_CLOUD_PROJECT (or the emulator env).
        self._client = firestore.AsyncClient(project=project or None)
        self._coll = collection

    def _col(self):  # type: ignore[no-untyped-def]
        return self._client.collection(self._coll)

    async def put(self, record: ParkRecord) -> None:
        await self._col().document(record.session_id).set(_doc_from_record(record))

    async def get(self, session_id: str) -> ParkRecord | None:
        snap = await self._col().document(session_id).get()
        if not snap.exists:
            return None
        return _record_from_doc(snap.to_dict() or {})

    async def resolve_by_pr_key(self, pr_key: str) -> ParkRecord | None:
        if pr_key == "":
            return None  # an empty key would match unparked docs (pr_key="")
        from google.cloud import firestore
        from google.cloud.firestore_v1.base_query import FieldFilter

        @firestore.async_transactional
        async def claim(transaction: AsyncTransaction) -> ParkRecord | None:
            query = self._col().where(filter=FieldFilter("pr_key", "==", pr_key)).limit(1)
            snaps = await query.get(transaction=transaction)
            if not snaps:
                return None
            return _claim_snapshot(transaction, snaps[0])

        return await claim(self._client.transaction())

    async def sweep(self, cutoff: datetime) -> list[ParkRecord]:
        from google.cloud.firestore_v1.base_query import FieldFilter

        # Collect candidates (parked + stale), then claim each in its own transaction so a
        # concurrent resolve cannot double-claim. parked_at is filtered in code (mirroring Go)
        # to avoid a composite index on (pr_key, parked_at).
        query = self._col().where(filter=FieldFilter("pr_key", "!=", ""))
        candidates: list[tuple[str, str]] = []
        async for snap in query.stream():
            d = snap.to_dict() or {}
            parked_at = d.get("parked_at")
            if parked_at is not None and parked_at < cutoff:
                candidates.append((d.get("session_id", ""), d.get("pr_key", "")))

        out: list[ParkRecord] = []
        for sid, pr_key in candidates:
            rec = await self._claim_by_session(sid)
            if rec is not None:
                rec.pr_key = pr_key  # restore for the caller (sweep needs the PR)
                out.append(rec)
        return out

    async def _claim_by_session(self, sid: str) -> ParkRecord | None:
        """The sweep's per-doc atomic claim, keyed by session id."""
        from google.cloud import firestore

        @firestore.async_transactional
        async def claim(transaction: AsyncTransaction) -> ParkRecord | None:
            ref = self._col().document(sid)
            snap = await ref.get(transaction=transaction)
            if not snap.exists:
                return None
            return _claim_snapshot(transaction, snap)

        return await claim(self._client.transaction())

    async def delete(self, session_id: str) -> None:
        await self._col().document(session_id).delete()

    async def parked_count(self) -> int:
        from google.cloud.firestore_v1.base_query import FieldFilter

        query = self._col().where(filter=FieldFilter("pr_key", "!=", ""))
        n = 0
        async for _ in query.stream():
            n += 1
        return n

    async def close(self) -> None:
        self._client.close()
