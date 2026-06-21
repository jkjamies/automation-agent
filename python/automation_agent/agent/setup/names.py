"""Shared naming helpers for agent construction."""

from __future__ import annotations


def safe_name(s: str) -> str:
    """Map ``s`` to an agent-name-safe string (non-ASCII-alphanumerics become ``_``).

    Used wherever a repo or file path is embedded in an ADK agent name (which must be a
    valid identifier-ish token).
    """
    out = []
    for ch in s:
        if ch.isascii() and ch.isalnum():
            out.append(ch)
        else:
            out.append("_")
    return "".join(out)
