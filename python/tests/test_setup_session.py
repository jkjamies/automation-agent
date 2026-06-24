"""Tests for the session-service factory (new_session_service)."""

from __future__ import annotations

import os

import pytest
from google.adk.sessions import InMemorySessionService

from automation_agent.agent.setup import new_session_service
from automation_agent.config import load_from

_needs_emulator = pytest.mark.skipif(
    not os.environ.get("FIRESTORE_EMULATOR_HOST"),
    reason="needs the Firestore emulator (FIRESTORE_EMULATOR_HOST)",
)


def test_memory_backend_returns_in_memory_service() -> None:
    cfg = load_from({"SESSION_BACKEND": "memory"}.get)
    assert isinstance(new_session_service(cfg), InMemorySessionService)


def test_sqlite_backend_returns_sqlite_service(tmp_path) -> None:
    from google.adk.sessions.sqlite_session_service import SqliteSessionService

    cfg = load_from({"SESSION_BACKEND": "sqlite", "SQLITE_DSN": str(tmp_path / "s.db")}.get)
    assert isinstance(new_session_service(cfg), SqliteSessionService)


@_needs_emulator
def test_firestore_backend_returns_firestore_service() -> None:
    # adk's NATIVE Firestore session service (not a hand-roll) — only the park store is custom.
    from google.adk.integrations.firestore.firestore_session_service import (
        FirestoreSessionService,
    )

    cfg = load_from({"SESSION_BACKEND": "firestore", "FIRESTORE_PROJECT": "test-project"}.get)
    assert isinstance(new_session_service(cfg), FirestoreSessionService)
