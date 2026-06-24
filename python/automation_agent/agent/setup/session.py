"""ADK session-service factory for the configured backend.

The session service holds the durable suspend/resume history that lets a parked fix run
continue after a process restart. Only the long-running fix loop needs durability;
ephemeral one-shot runners (explore/analyze/triage) keep using an in-memory session.

    memory    -> in-process; tests and ephemeral local runs (today's behavior, default)
    sqlite    -> file-backed via adk SqliteSessionService; durable local runs
    firestore -> cloud, adk's native google.adk.integrations.firestore service (later phase)

Keeping the backend imports here respects the ARCH boundary (infrastructure SDKs live
under ``agent.setup``).
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from google.adk.sessions import BaseSessionService, InMemorySessionService

if TYPE_CHECKING:
    from automation_agent.config import Config


def new_session_service(cfg: Config) -> BaseSessionService:
    """Build the ADK session service for the configured backend. sqlite and firestore land
    in later phases (firestore uses adk's native FirestoreSessionService, not a hand-roll —
    only the park store is custom)."""
    from automation_agent.config import SessionBackend

    if cfg.session_backend == SessionBackend.MEMORY:
        return InMemorySessionService()
    if cfg.session_backend == SessionBackend.SQLITE:
        # Imported lazily (and from the submodule — it is not re-exported from
        # google.adk.sessions) so the memory/firestore paths don't pull the sqlite backend.
        from google.adk.sessions.sqlite_session_service import SqliteSessionService

        return SqliteSessionService(db_path=cfg.sqlite_dsn)
    if cfg.session_backend == SessionBackend.FIRESTORE:  # pragma: no cover - emulator-only
        # adk ships a NATIVE Firestore session service (no hand-roll); only the park store
        # is custom. Lazily import it (and the firestore client) so other paths stay light.
        from google.adk.integrations.firestore.firestore_session_service import (
            FirestoreSessionService,
        )
        from google.cloud import firestore

        client = firestore.AsyncClient(project=cfg.firestore_project or None)
        return FirestoreSessionService(client=client, root_collection=cfg.firestore_collection)
    raise NotImplementedError(f"unknown session backend {cfg.session_backend!r}")
