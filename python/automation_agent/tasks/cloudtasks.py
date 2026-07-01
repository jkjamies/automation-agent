"""The Cloud Tasks execution transport — the production backend."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from datetime import UTC, datetime, timedelta
from typing import Any, Protocol

from google.cloud import tasks_v2

from automation_agent.ingest import Envelope, encode
from automation_agent.obs import inject as inject_trace

# MAX_TASK_BYTES is the Cloud Tasks size limit for an HTTP-target task (1 MiB; verify against
# current quota docs). enqueue refuses an envelope whose encoded body exceeds it rather than
# letting Cloud Tasks reject the create call opaquely (spec §9). Today's payloads are metadata
# well under this (PR diffs are fetched later via the API, not carried in the webhook body);
# if a future payload could exceed it, the fallback is store-in-Firestore + enqueue a
# reference — noted in the spec, not built here.
MAX_TASK_BYTES = 1 << 20


class Submitter(Protocol):
    """The slice of the Cloud Tasks async client this backend uses, isolated so task-building
    can be unit-tested against a fake without a live gRPC connection."""

    async def create_task(self, *, request: Any) -> Any: ...


class CloudTasks:
    """Enqueues each envelope as a Cloud Tasks HTTP-target task pointed at
    ``/internal/dispatch`` — the production backend.

    The queue gives durable retry with backoff (a task survives the instance being reclaimed
    mid-run and is redelivered) and rate limiting (the queue's max-concurrent-dispatches
    replaces the in-process semaphore), and the worker runs in-request so CPU stays allocated
    for the whole compute.
    """

    def __init__(
        self,
        client: Submitter,
        queue_path: str,
        dispatch_url: str,
        token: str,
        deadline: timedelta = timedelta(0),
        closer: Callable[[], Awaitable[None]] | None = None,
        now: Callable[[], datetime] | None = None,
    ) -> None:
        self._client = client
        self._queue_path = queue_path
        self._dispatch_url = dispatch_url
        self._token = token
        # Explicit per-task dispatch deadline. The HTTP-target default is only 10m, so a
        # longer workflow would be cancelled mid-run and retried (duplicating side effects).
        self._deadline = deadline
        self._closer = closer
        self._now = now if now is not None else (lambda: datetime.now(UTC))

    async def enqueue(
        self, e: Envelope, *, name: str = "", delay: timedelta = timedelta(0)
    ) -> None:
        """Build and submit a task carrying the JSON-encoded envelope as its body and the
        INTERNAL_TOKEN as a Bearer header. ``name`` sets the task name (Cloud Tasks dedup);
        ``delay`` sets the schedule time.

        Raises:
            ValueError: if the envelope's kind is unknown (rejected by :func:`encode`) or the
                encoded body exceeds the Cloud Tasks task-size limit.
        """
        body = encode(e)
        if len(body) > MAX_TASK_BYTES:
            raise ValueError(
                f"tasks: envelope is {len(body)} bytes, over the {MAX_TASK_BYTES}-byte "
                "Cloud Tasks task limit"
            )

        headers = {"Content-Type": "application/json"}
        if self._token:
            headers["Authorization"] = "Bearer " + self._token
        # Inject the active trace context as a W3C traceparent header so the /internal/dispatch
        # request that runs this task continues the ingress trace (the dispatch span becomes a
        # child of the ingress span). A no-op when tracing is disabled or no span is active —
        # no header is added, so the task wire format is unchanged.
        headers.update(inject_trace())
        task = tasks_v2.Task(
            http_request=tasks_v2.HttpRequest(
                http_method=tasks_v2.HttpMethod.POST,
                url=self._dispatch_url,
                headers=headers,
                body=body,
            )
        )
        # Set the dispatch deadline explicitly (the HTTP-target default is only 10m). Skipped
        # when unset (zero) so the queue default applies — production always supplies it.
        if self._deadline > timedelta(0):
            task.dispatch_deadline = self._deadline
        if name:
            task.name = self._queue_path + "/tasks/" + name
        if delay > timedelta(0):
            task.schedule_time = self._now() + delay

        request = tasks_v2.CreateTaskRequest(parent=self._queue_path, task=task)
        try:
            await self._client.create_task(request=request)
        except Exception as exc:  # noqa: BLE001
            raise RuntimeError(f"tasks: create task: {exc}") from exc

    async def close(self) -> None:
        """Release the underlying Cloud Tasks client (a no-op when none is set)."""
        if self._closer is not None:
            await self._closer()


def new_cloud_tasks(
    project: str,
    location: str,
    queue: str,
    dispatch_url: str,
    token: str,
    deadline: timedelta,
) -> CloudTasks:
    """Open a Cloud Tasks async client and target the queue
    ``projects/<project>/locations/<location>/queues/<queue>``.

    ``dispatch_url`` is the full URL of the ``/internal/dispatch`` worker; ``token`` is the
    static INTERNAL_TOKEN the task carries as a Bearer header (the same auth that endpoint
    already enforces). ``deadline`` is the explicit per-task dispatch deadline (config
    validated to Cloud Tasks' 15s..30m range). :meth:`CloudTasks.close` releases the client.
    """
    client = tasks_v2.CloudTasksAsyncClient()
    queue_path = client.queue_path(project, location, queue)

    async def _close() -> None:
        # The grpc_asyncio transport exposes an async close; releasing it frees the channel.
        await client.transport.close()

    return CloudTasks(
        client=client,
        queue_path=queue_path,
        dispatch_url=dispatch_url,
        token=token,
        deadline=deadline,
        closer=_close,
    )
