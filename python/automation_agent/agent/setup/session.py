"""ADK session-service factory for the configured backend.

The session service holds the durable suspend/resume history that lets a parked fix run
continue after a process restart. Only the long-running fix loop needs durability;
ephemeral one-shot runners (explore/analyze/triage) keep using an in-memory session.

    memory    -> in-process; tests and ephemeral local runs (today's behavior, default)
    sqlite    -> file-backed via adk SqliteSessionService; durable local runs (later phase)
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
    raise NotImplementedError(
        f"session backend {cfg.session_backend!r} not yet implemented "
        "(sqlite/firestore land in a later phase); use SESSION_BACKEND=memory"
    )
