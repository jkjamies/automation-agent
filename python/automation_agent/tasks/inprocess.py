"""The in-process execution transport — the local-dev and default backend."""

from __future__ import annotations

import asyncio
import logging
from datetime import timedelta

from automation_agent.ingest import Envelope
from automation_agent.tasks.transport import DispatchFunc

# DEFAULT_MAX_CONCURRENT bounds in-flight in-process dispatches under burst (backpressure).
DEFAULT_MAX_CONCURRENT = 32

# DRAIN_TIMEOUT caps how long close() waits for in-flight dispatches to finish (seconds).
DRAIN_TIMEOUT = 15


class InProcess:
    """Runs each envelope in a background asyncio task on a bounded pool — the local-dev and
    default backend.

    It reproduces the pre-transport behavior exactly: a burst applies backpressure (the
    bounded semaphore), and a clean SIGTERM drains in-flight work via :meth:`close`. It does
    NOT survive an instance being reclaimed mid-run, which is precisely why production uses
    the Cloud Tasks backend instead. The ``name`` / ``delay`` hints are Cloud Tasks features
    and are ignored here (an immediate, undeduplicated dispatch).
    """

    def __init__(
        self,
        dispatch: DispatchFunc,
        log: logging.Logger | None = None,
        max_concurrent: int = DEFAULT_MAX_CONCURRENT,
    ) -> None:
        self._dispatch = dispatch
        self._log = log if log is not None else logging.getLogger("automation_agent")
        if max_concurrent < 1:
            max_concurrent = DEFAULT_MAX_CONCURRENT
        self._max_concurrent = max_concurrent
        # Caps in-flight dispatches (matches Go's sem-32 channel). Acquired in enqueue before
        # the task is spawned, so a burst blocks the handler (backpressure) instead of piling
        # up tasks; released when the dispatch finishes.
        self._sem = asyncio.Semaphore(max_concurrent)
        # In-flight dispatch tasks. CPython holds only a weak reference to a bare task from
        # ``create_task``, so a fire-and-forget task can be garbage-collected mid-flight
        # ("Task was destroyed but it is pending!"). Keeping a strong reference here both
        # prevents that and lets close() drain outstanding work instead of dropping it.
        self._pending: set[asyncio.Task[None]] = set()
        # Set by close() to stop accepting new work before the drain, so enqueue cannot spawn
        # a task the drain would miss.
        self._closed = False

    async def enqueue(
        self, e: Envelope, *, name: str = "", delay: timedelta = timedelta(0)
    ) -> None:
        """Dispatch ``e`` on the bounded pool. Blocks while the pool is full (backpressure
        under burst) and otherwise returns immediately; the dispatch error is logged, not
        raised, because the webhook response has already gone out. ``name`` / ``delay`` are
        ignored (Cloud Tasks features)."""
        if self._closed:
            # Shutdown has begun: refuse new work rather than spawn a task the drain has
            # already stopped waiting for (it would be abandoned on exit).
            raise RuntimeError("tasks: in-process transport is closed")
        # When every slot is held, acquire() blocks here — the intended backpressure. Surface
        # it so sustained saturation is observable rather than silent (delayed webhook ACKs).
        if self._sem.locked():
            self._log.warning(
                "dispatch concurrency saturated (%d in flight); webhook ingest is applying "
                "backpressure until a slot frees",
                self._max_concurrent,
            )
        await self._sem.acquire()
        # Recheck after the (possibly long) backpressure wait: close() may have begun while we
        # were blocked on the semaphore. Without this, a task could slip past the drain's
        # snapshot and be abandoned on exit. (Mirrors Go's second select on the closed channel.)
        if self._closed:
            self._sem.release()
            raise RuntimeError("tasks: in-process transport is closed")
        task = asyncio.get_running_loop().create_task(self._dispatch_and_release(e))
        self._pending.add(task)
        task.add_done_callback(self._pending.discard)

    async def _dispatch_and_release(self, e: Envelope) -> None:
        # The dispatch runs detached from the originating webhook request (already returned),
        # so cancelling that request does not cancel the dispatch. The error is logged, not
        # raised, because the response has already gone out.
        try:
            await self._dispatch(e)
        except Exception as exc:  # noqa: BLE001
            self._log.error("dispatch failed: kind=%s source=%s err=%s", e.kind, e.source, exc)
        finally:
            self._sem.release()

    async def close(self) -> None:
        """Drain in-flight dispatches (bounded by :data:`DRAIN_TIMEOUT`) so a clean SIGTERM
        finishes work in flight rather than abandoning it."""
        # Stop accepting new work before waiting, so enqueue cannot spawn a task the drain
        # would miss.
        self._closed = True
        if not self._pending:
            return
        self._log.info("draining %d in-flight dispatch(es)", len(self._pending))
        # Wait on a snapshot with asyncio.wait (not wait_for+gather): on timeout it only stops
        # waiting and does NOT cancel the still-running dispatches, matching Go's Close, which
        # lets in-flight goroutines run to completion past the drain deadline.
        _, still_pending = await asyncio.wait(set(self._pending), timeout=DRAIN_TIMEOUT)
        if still_pending:
            self._log.warning("drain timed out; %d dispatch(es) abandoned", len(still_pending))
            return
        self._log.info("drained in-flight work")
