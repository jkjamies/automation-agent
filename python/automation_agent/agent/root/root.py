"""The dispatcher kicked off for every ingest.

It routes a normalized :class:`~automation_agent.ingest.Envelope` to the right workflow
by :class:`~automation_agent.ingest.Kind`. Keeping a single entry point is why "root"
exists: new ingress sources and smarter (e.g. LLM-based) routing slot in here without
restructuring.
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable

from automation_agent.ingest import Envelope, Kind

# Handler runs the work for one ingest envelope; errors raise instead of being
# returned.
Handler = Callable[[Envelope], Awaitable[None]]


class Dispatcher:
    """Routes envelopes to handlers by Kind."""

    def __init__(self, log: logging.Logger | None = None) -> None:
        """Create an empty dispatcher."""
        self._handlers: dict[Kind, Handler] = {}
        self._log = log if log is not None else logging.getLogger(__name__)

    def register(self, kind: Kind, handler: Handler) -> None:
        """Bind a handler to a kind (last registration wins)."""
        self._handlers[kind] = handler

    def handles(self, kind: Kind) -> bool:
        """Report whether a kind has a registered handler."""
        return kind in self._handlers

    async def dispatch(self, e: Envelope) -> None:
        """Route one envelope.

        An unregistered kind is logged and ignored, so an ingress that isn't wired yet
        is a no-op, not a crash.
        """
        handler = self._handlers.get(e.kind)
        if handler is None:
            self._log.warning(
                "no handler for ingest kind", extra={"kind": e.kind, "source": e.source}
            )
            return
        self._log.info(
            "dispatching", extra={"kind": e.kind, "source": e.source}
        )
        await handler(e)
