"""Tests for the root dispatcher (mirror of ``root_test.go``)."""

from __future__ import annotations

from collections.abc import AsyncIterator
from datetime import UTC, datetime

import pytest
from google.adk.agents import BaseAgent
from google.adk.agents.invocation_context import InvocationContext
from google.adk.events import Event
from typing_extensions import override

from automation_agent.agent import setup
from automation_agent.agent.root import Deps, Dispatcher, build_root_dispatcher
from automation_agent.ingest import Envelope, Kind, new


def env(kind: Kind) -> Envelope:
    return new(kind, "test", b"", datetime.fromtimestamp(1, tz=UTC))


async def test_dispatch_routes_by_kind() -> None:
    d = Dispatcher()
    got: list[Kind] = []

    async def handler(e: Envelope) -> None:
        got.append(e.kind)

    d.register(Kind.CRON_DAILY, handler)

    assert d.handles(Kind.CRON_DAILY)
    await d.dispatch(env(Kind.CRON_DAILY))
    assert got == [Kind.CRON_DAILY]


async def test_dispatch_unhandled_is_no_op() -> None:
    d = Dispatcher()
    assert not d.handles(Kind.LINT)
    # Unhandled kind must not raise.
    await d.dispatch(env(Kind.LINT))


async def test_dispatch_propagates_handler_error() -> None:
    d = Dispatcher()

    async def handler(e: Envelope) -> None:
        raise RuntimeError("handler failed")

    d.register(Kind.CI, handler)
    with pytest.raises(RuntimeError, match="handler failed"):
        await d.dispatch(env(Kind.CI))


class _TrivialAgent(BaseAgent):
    """A code agent that emits one event, used to build a real runner without an LLM."""

    @override
    async def _run_async_impl(
        self, ctx: InvocationContext
    ) -> AsyncIterator[Event]:
        yield setup.text_event(self.name, "done")


async def test_build_root_dispatcher_with_summary() -> None:
    d = build_root_dispatcher(Deps(summary_agent=_TrivialAgent(name="trivial")))
    assert d.handles(Kind.CRON_DAILY)
    assert d.handles(Kind.CRON_WEEKLY)
    # Drive the summary handler through a real runner (no LLM).
    await d.dispatch(env(Kind.CRON_DAILY))


async def test_build_root_dispatcher_fix_handlers() -> None:
    called: dict[Kind, bool] = {}

    async def mark(e: Envelope) -> None:
        called[e.kind] = True

    d = build_root_dispatcher(
        Deps(lint_kickoff=mark, coverage_kickoff=mark, ci_resume=mark)
    )
    assert d.handles(Kind.LINT)
    assert d.handles(Kind.COVERAGE)
    assert d.handles(Kind.CI)
    for k in (Kind.LINT, Kind.COVERAGE, Kind.CI):
        await d.dispatch(env(k))
        assert called[k]


async def test_build_root_dispatcher_without_summary() -> None:
    d = build_root_dispatcher(Deps(summary_agent=None))
    assert not d.handles(Kind.CRON_DAILY)
    assert not d.handles(Kind.CRON_WEEKLY)
