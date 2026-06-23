"""Tests for the session-service factory (new_session_service)."""

from __future__ import annotations

import pytest
from google.adk.sessions import InMemorySessionService

from automation_agent.agent.setup import new_session_service
from automation_agent.config import load_from


def test_memory_backend_returns_in_memory_service() -> None:
    cfg = load_from({"SESSION_BACKEND": "memory"}.get)
    assert isinstance(new_session_service(cfg), InMemorySessionService)


def test_unimplemented_backend_raises() -> None:
    # sqlite/firestore land in later phases; until then they fail loudly rather than
    # silently degrading to in-memory.
    cfg = load_from({"SESSION_BACKEND": "firestore"}.get)
    with pytest.raises(NotImplementedError):
        new_session_service(cfg)
