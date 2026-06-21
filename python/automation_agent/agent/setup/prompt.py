"""Markdown prompt loader.

Each agent ships its own ``prompts/`` directory of markdown files and reads them
through :class:`Prompts`, so prompts stay reviewable next to the agent that uses
them. Prompts are loaded with ``importlib.resources`` against the agent's package.
"""

from __future__ import annotations

from importlib import resources


class Prompts:
    """Loads ``prompts/<name>.md`` from a given package anchor."""

    def __init__(self, anchor: str) -> None:
        """anchor is the dotted package name whose ``prompts/`` dir holds the files,
        e.g. ``"automation_agent.agent.summary"``."""
        self._anchor = anchor

    def get(self, name: str) -> str:
        """Return the trimmed contents of ``prompts/<name>.md``.

        Raises:
            FileNotFoundError / OSError: if the prompt file is missing.
        """
        try:
            path = resources.files(self._anchor).joinpath("prompts", f"{name}.md")
            return path.read_text(encoding="utf-8").strip()
        except (FileNotFoundError, ModuleNotFoundError) as exc:
            raise FileNotFoundError(f"read prompt {name!r}: {exc}") from exc

    def must_get(self, name: str) -> str:
        """Like :meth:`get`, but intended for agent-construction time where a missing
        prompt is a programming error that should fail fast at startup."""
        return self.get(name)
