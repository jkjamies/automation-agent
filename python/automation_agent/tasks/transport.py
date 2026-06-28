"""The execution transport between webhook ingress and the dispatcher.

Webhook ingress reduces a request to an :class:`~automation_agent.ingest.Envelope` and
calls :meth:`Transport.enqueue`, which returns fast; the envelope's workflow runs *later* —
in a background task for the in-process backend, or in a fresh ``/internal/dispatch``
request delivered by Cloud Tasks in production. The seam exists because on Cloud Run with
request-based billing CPU is throttled to near-zero once a response is sent, so multi-minute
LLM compute must run *inside* a request (Cloud Tasks gives that, plus durable retry and rate
limiting). See ``specs/20260626-workflow-execution-transport.md``. Deterministic tooling —
no agent imports (the dispatcher is injected as a ``DispatchFunc``).
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from datetime import timedelta
from typing import Protocol

from automation_agent.ingest import Envelope

# DispatchFunc runs the work for one envelope. It is the root dispatcher's ``dispatch``,
# passed in so this package stays decoupled from the agent layer.
DispatchFunc = Callable[[Envelope], Awaitable[None]]


class Transport(Protocol):
    """Enqueues an envelope for asynchronous execution and returns quickly.

    A raised error becomes a 500 to the webhook caller (so GitHub / Cloud Scheduler
    retries). The optional ``name`` / ``delay`` hints are backend-honored: the transport
    stays deliberately dumb about workflow semantics and carries them without interpreting
    them — coalesce-to-latest / staleness logic lives in the workflow, not here (spec
    Decision §3). ``name`` is a Cloud Tasks dedup key (a duplicate task with the same name
    is dropped for ~1h, giving idempotency against a redelivered webhook); ``delay``
    schedules delivery that far in the future (e.g. a review debounce window). Both are
    Cloud-Tasks-only — the in-process backend ignores them (an immediate, undeduplicated
    dispatch).
    """

    async def enqueue(
        self, e: Envelope, *, name: str = "", delay: timedelta = timedelta(0)
    ) -> None:
        """Schedule ``e`` for execution. ``name`` / ``delay`` are optional, backend-honored
        hints."""
        ...

    async def close(self) -> None:
        """Release the backend: the in-process backend drains in-flight tasks; the Cloud
        Tasks backend closes its gRPC client. Safe to call once at shutdown."""
        ...
