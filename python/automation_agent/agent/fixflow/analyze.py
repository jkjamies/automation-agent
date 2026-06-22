"""Per-file parallel analysis.

:func:`parallel_analyze` fans out one analyzer agent per file (ADK parallel agents,
each writing distinct state keys so they never collide), calls the edit function for
each, and returns the collected non-empty edits sorted by path. The state-key scheme is
``edit:<workPath>`` -> new content and ``path:<workPath>`` -> target edit path.
"""

from __future__ import annotations

import logging
from collections.abc import AsyncGenerator, Awaitable, Callable
from typing import cast

from google.adk.agents import BaseAgent, ParallelAgent
from google.adk.agents.invocation_context import InvocationContext
from google.adk.events import Event
from typing_extensions import override

from automation_agent.agent import setup
from automation_agent.agent.fixflow.applyfix import FileEdit
from automation_agent.agent.fixflow.engine import FileWork

log = logging.getLogger(__name__)

EDIT_PREFIX = "edit:"  # state key per file: edit:<workPath> -> new content
PATH_PREFIX = "path:"  # state key per file: path:<workPath> -> target edit path

# EditFunc produces the edit for one file's work: a target path (which may differ from
# the source path — e.g. a test file) and new content. Return a zero FileEdit (empty
# path or content) to skip this file.
EditFunc = Callable[[FileWork], Awaitable[FileEdit]]


class _Analyzer(BaseAgent):
    """A BaseAgent that runs the edit function for one file and emits its edit as a
    state delta (or skips / errors)."""

    _work: FileWork
    _fn: EditFunc
    model_config = {"arbitrary_types_allowed": True}

    def __init__(self, name: str, work: FileWork, fn: EditFunc) -> None:
        super().__init__(name=name, description=f"Analyzes {work.path}")
        object.__setattr__(self, "_work", work)
        object.__setattr__(self, "_fn", fn)

    @override
    async def _run_async_impl(
        self, ctx: InvocationContext
    ) -> AsyncGenerator[Event, None]:
        w = self._work
        try:
            edit = await self._fn(w)
        except Exception as exc:  # noqa: BLE001
            log.warning("analyze %s failed: %s", w.path, exc)
            yield setup.text_event(self.name, f"analyze {w.path}: {exc}")
            return
        if edit.path == "" or edit.content.strip() == "":
            yield setup.text_event(self.name, f"skipped {w.path}")
            return
        yield setup.text_event(
            self.name,
            f"edited {edit.path}",
            {EDIT_PREFIX + w.path: edit.content, PATH_PREFIX + w.path: edit.path},
        )


def _unique_analyzer_name(seen: dict[str, int], path: str) -> str:
    """Derive a unique sub-agent name from a path. ``safe_name`` maps every non-alphanumeric
    char to ``_``, so distinct paths (e.g. ``a/b.kt`` and ``a-b.kt``) can collapse to the same
    name; ParallelAgent needs unique sub-agent names, so a collision gets a numeric suffix —
    otherwise one analyzer silently shadows another and that file's edits are dropped."""
    base = "analyze_" + setup.safe_name(path)
    seen[base] = seen.get(base, 0) + 1
    n = seen[base]
    return f"{base}_{n}" if n > 1 else base


async def parallel_analyze(work: list[FileWork], fn: EditFunc) -> list[FileEdit]:
    """Fan out one analyzer per FileWork, call ``fn`` for each, and return the collected
    non-empty edits sorted by path."""
    if not work:
        raise ValueError("analyze: no files to work on")
    sorted_work = sorted(work, key=lambda w: w.path)

    seen: dict[str, int] = {}
    agents: list[BaseAgent] = [
        _Analyzer(_unique_analyzer_name(seen, w.path), w, fn) for w in sorted_work
    ]
    par = ParallelAgent(
        name="analyze_all",
        description="Per-file analysis in parallel",
        sub_agents=agents,
    )
    runner = setup.new_runner("fix-analyze", par)
    state = await setup.drive_collect_state(
        runner, "system", "analyze", "Produce the edits."
    )

    edits: list[FileEdit] = []
    for w in sorted_work:
        content = cast(str, state.get(EDIT_PREFIX + w.path) or "")
        path = cast(str, state.get(PATH_PREFIX + w.path) or "")
        if isinstance(content, str) and content.strip() != "" and path != "":
            edits.append(FileEdit(path=path, content=content))
    if not edits:
        raise ValueError("analyze produced no edits")
    return edits
