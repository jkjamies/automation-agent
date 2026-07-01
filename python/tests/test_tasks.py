"""Tests for the execution transport (in-process + Cloud Tasks backends)."""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest
from google.cloud import tasks_v2

from automation_agent.ingest import Envelope, Kind, decode, encode, new
from automation_agent.tasks import (
    DEFAULT_MAX_CONCURRENT,
    MAX_TASK_BYTES,
    CloudTasks,
    InProcess,
)
from automation_agent.tasks import inprocess as inprocess_mod


def _env(kind: Kind = Kind.LINT, payload: bytes = b"x") -> Envelope:
    return new(kind, "webhook:/lint", payload, datetime.fromtimestamp(0, tz=UTC))


# --- InProcess ---------------------------------------------------------------


async def test_inprocess_dispatches() -> None:
    got: list[Envelope] = []
    done = asyncio.Event()

    async def dispatch(e: Envelope) -> None:
        got.append(e)
        done.set()

    p = InProcess(dispatch, max_concurrent=4)
    await p.enqueue(_env(Kind.LINT))
    await asyncio.wait_for(done.wait(), timeout=2)
    await p.close()
    assert got and got[0].kind == Kind.LINT


async def test_inprocess_swallows_dispatch_error() -> None:
    # A dispatch error is logged, not raised (the webhook response has already gone out), so
    # enqueue still succeeds.
    done = asyncio.Event()

    async def dispatch(_e: Envelope) -> None:
        done.set()
        raise RuntimeError("boom")

    p = InProcess(dispatch, max_concurrent=1)
    await p.enqueue(_env(Kind.CI))  # must not raise
    await asyncio.wait_for(done.wait(), timeout=2)
    await p.close()


async def test_inprocess_constructor_defaults() -> None:
    # A non-positive max_concurrent and a None logger fall back to defaults; the pool still
    # dispatches.
    done = asyncio.Event()
    p = InProcess(lambda _e: _set(done), log=None, max_concurrent=0)
    assert p._max_concurrent == DEFAULT_MAX_CONCURRENT
    await p.enqueue(_env())
    await asyncio.wait_for(done.wait(), timeout=2)
    await p.close()


async def _set(ev: asyncio.Event) -> None:
    ev.set()


async def test_inprocess_close_drains() -> None:
    # A dispatch still running when close() is called completes before close() returns.
    release = asyncio.Event()
    finished = asyncio.Event()

    async def dispatch(_e: Envelope) -> None:
        await release.wait()
        finished.set()

    p = InProcess(dispatch, max_concurrent=1)
    await p.enqueue(_env(Kind.CRON_DAILY))

    closing = asyncio.ensure_future(p.close())
    # close() must still be waiting while the dispatch is blocked.
    await asyncio.sleep(0.05)
    assert not closing.done()
    release.set()
    await asyncio.wait_for(closing, timeout=2)
    assert finished.is_set()


async def test_inprocess_enqueue_after_close_is_rejected() -> None:
    ran = asyncio.Event()
    p = InProcess(lambda _e: _set(ran), max_concurrent=1)
    await p.close()
    with pytest.raises(RuntimeError):
        await p.enqueue(_env(Kind.CI))
    assert not ran.is_set()


async def test_inprocess_enqueue_rejected_if_closed_during_backpressure() -> None:
    # An enqueue parked on the semaphore when close() begins must back out (release its slot
    # and raise) once it acquires, rather than spawn a task the drain has already snapshotted
    # past — the recheck-after-acquire guard (re-checks the closed flag after acquiring).
    # Drives the real close() concurrently rather than poking _closed directly.
    started = asyncio.Event()
    release = asyncio.Event()

    async def dispatch(_e: Envelope) -> None:
        started.set()
        await release.wait()

    p = InProcess(dispatch, max_concurrent=1)
    await p.enqueue(_env())  # occupies the only slot
    await started.wait()  # dispatch #1 is running, slot held
    # second passes its initial closed-check (still open), then parks on acquire().
    second = asyncio.ensure_future(p.enqueue(_env()))
    await asyncio.sleep(0)  # let second reach acquire()
    # Real shutdown: close() sets _closed, then drains the in-flight dispatch.
    closing = asyncio.ensure_future(p.close())
    await asyncio.sleep(0)  # let close() mark _closed and start draining
    release.set()  # dispatch #1 finishes -> slot frees -> second acquires, rechecks _closed
    with pytest.raises(RuntimeError):
        await second
    await closing


async def test_inprocess_close_timeout_does_not_cancel(monkeypatch) -> None:
    # On drain timeout, close() only stops waiting — it must NOT cancel the still-running
    # dispatch; in-flight work is left to run to completion past the drain deadline.
    monkeypatch.setattr(inprocess_mod, "DRAIN_TIMEOUT", 0.05)
    release = asyncio.Event()
    finished = asyncio.Event()

    async def dispatch(_e: Envelope) -> None:
        await release.wait()
        finished.set()

    p = InProcess(dispatch, max_concurrent=1)
    await p.enqueue(_env())
    inflight = set(p._pending)
    await p.close()  # times out after 0.05s
    assert not finished.is_set()  # still running, not cancelled
    for task in inflight:
        assert not task.cancelled()
    release.set()
    await asyncio.gather(*inflight)
    assert finished.is_set()


async def test_inprocess_applies_backpressure() -> None:
    # With the pool full, a second enqueue blocks (backpressure) until a slot frees.
    release = asyncio.Event()

    async def dispatch(_e: Envelope) -> None:
        await release.wait()

    p = InProcess(dispatch, max_concurrent=1)
    await p.enqueue(_env())  # occupies the only slot
    second = asyncio.ensure_future(p.enqueue(_env()))
    await asyncio.sleep(0.05)
    assert not second.done()  # blocked on the semaphore
    release.set()
    await asyncio.wait_for(second, timeout=2)
    await p.close()


# --- CloudTasks --------------------------------------------------------------


class FakeSubmitter:
    """Records the last CreateTaskRequest and returns a configurable error."""

    def __init__(self, err: Exception | None = None) -> None:
        self.request: Any = None
        self.err = err

    async def create_task(self, *, request: Any) -> Any:
        self.request = request
        if self.err is not None:
            raise self.err
        return request.task


def _new_cloudtasks(f: FakeSubmitter, token: str) -> CloudTasks:
    """Build a CloudTasks over a fake submitter with a fixed clock, so task building is
    exercised without a live gRPC client."""
    return CloudTasks(
        client=f,
        queue_path="projects/p/locations/l/queues/q",
        dispatch_url="https://svc.run.app/internal/dispatch",
        token=token,
        deadline=timedelta(minutes=30),
        now=lambda: datetime.fromtimestamp(1_700_000_000, tz=UTC),
    )


def _http_request(task: tasks_v2.Task) -> tasks_v2.HttpRequest:
    return task.http_request


async def test_cloudtasks_builds_task() -> None:
    # A plain enqueue builds a POST task targeting /internal/dispatch, carrying the encoded
    # envelope as the body and the INTERNAL_TOKEN as a Bearer header, with no name/schedule.
    f = FakeSubmitter()
    ct = _new_cloudtasks(f, "sekret")
    env = new(
        Kind.CI, "webhook:/github", b'{"action":"completed"}', datetime.fromtimestamp(0, tz=UTC)
    )
    await ct.enqueue(env)

    req = f.request
    assert req.parent == "projects/p/locations/l/queues/q"
    hr = _http_request(req.task)
    assert hr.http_method == tasks_v2.HttpMethod.POST
    assert hr.url == "https://svc.run.app/internal/dispatch"
    assert hr.headers["Authorization"] == "Bearer sekret"
    assert hr.headers["Content-Type"] == "application/json"
    # The body is the exact wire codec output and decodes back to the envelope.
    assert bytes(hr.body) == encode(env)
    assert decode(hr.body).kind == Kind.CI
    # No dedup name / schedule requested.
    assert req.task.name == ""
    assert not tasks_v2.Task.pb(req.task).HasField("schedule_time")
    # The dispatch deadline is set explicitly (so a long workflow is not cancelled at the
    # HTTP-target default of 10m and retried, duplicating side effects).
    assert tasks_v2.Task.pb(req.task).HasField("dispatch_deadline")
    assert req.task.dispatch_deadline == timedelta(minutes=30)


async def test_cloudtasks_omits_deadline_when_unset() -> None:
    # With no deadline configured (zero) the task omits dispatch_deadline so the queue default
    # applies — production always supplies a config-validated value.
    f = FakeSubmitter()
    ct = _new_cloudtasks(f, "")
    ct._deadline = timedelta(0)
    await ct.enqueue(_env(Kind.CI, b""))
    assert not tasks_v2.Task.pb(f.request.task).HasField("dispatch_deadline")


async def test_cloudtasks_honors_name_and_delay() -> None:
    # The optional dedup name and schedule delay are carried onto the built task.
    f = FakeSubmitter()
    ct = _new_cloudtasks(f, "")
    await ct.enqueue(_env(Kind.COVERAGE, b"{}"), name="pr-42", delay=timedelta(seconds=30))

    assert f.request.task.name == "projects/p/locations/l/queues/q/tasks/pr-42"
    assert tasks_v2.Task.pb(f.request.task).HasField("schedule_time")
    assert f.request.task.schedule_time == datetime.fromtimestamp(1_700_000_030, tz=UTC)
    # With no token configured, no Authorization header is attached.
    assert "Authorization" not in _http_request(f.request.task).headers


async def test_cloudtasks_rejects_oversize_envelope() -> None:
    # An envelope whose encoded body exceeds the Cloud Tasks task-size limit is refused up
    # front rather than failing opaquely at create time (spec §9).
    f = FakeSubmitter()
    ct = _new_cloudtasks(f, "")
    big = new(Kind.LINT, "s", b"x" * (MAX_TASK_BYTES + 1), datetime.fromtimestamp(0, tz=UTC))
    with pytest.raises(ValueError, match="task limit"):
        await ct.enqueue(big)
    assert f.request is None  # never reached create_task


async def test_cloudtasks_surfaces_submit_error() -> None:
    # A create failure surfaces to the caller (which becomes a 500 -> the webhook source
    # retries, and the queue itself retries an /internal/dispatch failure).
    f = FakeSubmitter(err=RuntimeError("unavailable"))
    ct = _new_cloudtasks(f, "")
    with pytest.raises(RuntimeError, match="create task"):
        await ct.enqueue(_env(Kind.CI, b""))


async def test_cloudtasks_injects_traceparent(otel_recording: Any) -> None:
    # With tracing on and a span active, enqueue injects the trace context as a W3C
    # traceparent header so the /internal/dispatch request continues the ingress trace.
    from opentelemetry import context as context_api
    from opentelemetry import trace

    f = FakeSubmitter()
    ct = _new_cloudtasks(f, "sekret")
    span = trace.get_tracer("obs-test").start_span("ingress")
    token = context_api.attach(trace.set_span_in_context(span))
    try:
        await ct.enqueue(_env(Kind.CI, b"{}"))
    finally:
        context_api.detach(token)
        span.end()

    headers = _http_request(f.request.task).headers
    assert "traceparent" in headers
    # The injected traceparent carries the active span's trace id (32 lowercase hex chars).
    trace_id = trace.format_trace_id(span.get_span_context().trace_id)
    assert trace_id in headers["traceparent"]
    # Injection is additive: the existing auth/content headers are untouched.
    assert headers["Authorization"] == "Bearer sekret"


async def test_cloudtasks_no_traceparent_when_disabled() -> None:
    # With tracing disabled (no provider/span), no traceparent header leaks onto the task.
    f = FakeSubmitter()
    ct = _new_cloudtasks(f, "")
    await ct.enqueue(_env(Kind.CI, b"{}"))
    assert "traceparent" not in _http_request(f.request.task).headers


async def test_cloudtasks_close() -> None:
    # Close releases the underlying client, and is a no-op when none is set.
    closed = asyncio.Event()
    ct = CloudTasks(
        client=FakeSubmitter(),
        queue_path="q",
        dispatch_url="https://x/internal/dispatch",
        token="",
        closer=lambda: _set(closed),
    )
    await ct.close()
    assert closed.is_set()
    # No closer -> no-op.
    await CloudTasks(
        client=FakeSubmitter(), queue_path="q", dispatch_url="https://x/internal/dispatch", token=""
    ).close()
