"""In-memory parked-run registry — the fix-loop spine.

Tracks suspended fix runs keyed by PR. Exactly one of
{CI webhook, timeout timer} ever resolves a given run: :meth:`RunRegistry.resolve`
removes the entry, so late/duplicate deliveries find nothing and no-op. The registry IS
the in-flight record — no DB, no PR scan. Parked runs live only in memory.

The whole driver runs in one asyncio event loop, so the timer is an ``asyncio``
``call_later`` handle and ``resolve`` is naturally atomic (no preemption between dict
lookup and delete). The TimerHandle is stored on the ParkedRun so resolve can cancel
it.
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any


@dataclass
class ParkedRun:
    """One suspended fix run awaiting its CI result. ``session_id`` + ``call_id`` are
    what a resume needs to feed the CI outcome back into the parked run."""

    session_id: str
    call_id: str
    attempts: int = 0
    _timer: Any | None = field(default=None, repr=False, compare=False)


class RunRegistry:
    """Tracks parked runs in memory, keyed by PR."""

    def __init__(self) -> None:
        self._runs: dict[str, ParkedRun] = {}
        # Strong refs to in-flight timeout tasks. CPython only weakly references a bare
        # ensure_future task, so without this a fired timeout handler (which frees the run
        # and notifies for review) could be garbage-collected before it completes.
        self._timeout_tasks: set[asyncio.Future[None]] = set()

    def park(
        self,
        pr_key: str,
        run: ParkedRun,
        timeout: timedelta,
        on_timeout: Callable[[str], Awaitable[None]],
    ) -> None:
        """Record a parked run for ``pr_key`` and arm its timeout. ``on_timeout`` fires
        once if the run is still parked when ``timeout`` elapses; it must call
        :meth:`resolve` to claim the run (and loses the claim if a webhook got there
        first). A prior parking for the same key (e.g. a retry re-park) is replaced and
        its timer cancelled."""
        old = self._runs.get(pr_key)
        if old is not None and old._timer is not None:
            old._timer.cancel()
        loop = asyncio.get_running_loop()

        def _arm() -> None:
            task = asyncio.ensure_future(on_timeout(pr_key))
            self._timeout_tasks.add(task)
            task.add_done_callback(self._timeout_tasks.discard)

        run._timer = loop.call_later(timeout.total_seconds(), _arm)
        self._runs[pr_key] = run

    def resolve(self, pr_key: str) -> ParkedRun | None:
        """Atomically claim and remove the parked run for ``pr_key``, cancelling its
        timer. Returns the run for the single winner; ``None`` for late/duplicate/unknown
        callers."""
        run = self._runs.pop(pr_key, None)
        if run is None:
            return None
        if run._timer is not None:
            run._timer.cancel()
        return run

    def len(self) -> int:
        """Report the number of currently parked runs."""
        return len(self._runs)
