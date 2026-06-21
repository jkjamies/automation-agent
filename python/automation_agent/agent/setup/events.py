"""Small genai/ADK content + event helpers used by code agents.

Code agents use :func:`text_event` to emit model-authored output and write workflow
state in one shot.
"""

from __future__ import annotations

from typing import Any

from google.adk.events import Event, EventActions
from google.genai import types

ROLE_USER = "user"
ROLE_MODEL = "model"


def user_text(text: str) -> types.Content:
    """Build a user-role content message from plain text (seeds an invocation)."""
    return types.Content(role=ROLE_USER, parts=[types.Part.from_text(text=text)])


def assistant_text(text: str) -> types.Content:
    """Build a model-role content message from plain text."""
    return types.Content(role=ROLE_MODEL, parts=[types.Part.from_text(text=text)])


def content_text(content: types.Content | None) -> str:
    """Concatenate the text parts of a content (None-safe)."""
    if content is None or content.parts is None:
        return ""
    return "".join(p.text for p in content.parts if p.text)


def last_text(contents: list[types.Content]) -> str:
    """Return the concatenated text of the final content in a list, or ""."""
    if not contents:
        return ""
    return content_text(contents[-1])


def text_event(author: str, text: str, state: dict[str, Any] | None = None) -> Event:
    """Build an Event carrying model-authored text, optionally with a state delta."""
    actions = EventActions(state_delta=dict(state)) if state else EventActions()
    return Event(author=author, content=assistant_text(text), actions=actions)


def state_string(state: Any, key: str) -> str:
    """Return the string value at ``key``, or "" if absent or not a string.

    ``state`` is any mapping-like session state (``dict`` or ADK State / ReadonlyState).
    """
    try:
        value = state.get(key)
    except Exception:
        return ""
    return value if isinstance(value, str) else ""
