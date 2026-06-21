"""Builds the root dispatcher and registers available workflows (port of
``root/agents_setup.go``).

Cron kinds → summary; LINT → lint-fixer; COVERAGE → coverage-fixer; CI → resume
(all fix engines). Each handler is optional.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass

from google.adk.agents import BaseAgent
from google.adk.runners import Runner

from automation_agent.agent import setup
from automation_agent.agent.root.root import Dispatcher, Handler
from automation_agent.ingest import Envelope, Kind


@dataclass
class Deps:
    """Wires the dispatcher. Each handler is optional.

    ``ci_resume`` handles :attr:`Kind.CI` for every fix workflow (lint, coverage) — each
    engine no-ops unless its check matches.
    """

    summary_agent: BaseAgent | None = None
    lint_kickoff: Handler | None = None  # Kind.LINT
    coverage_kickoff: Handler | None = None  # Kind.COVERAGE
    ci_resume: Handler | None = None  # Kind.CI (dispatched to all fix engines)
    log: logging.Logger | None = None


def build_root_dispatcher(d: Deps) -> Dispatcher:
    """Build the dispatcher and register the available workflows.

    Cron kinds → summary; LINT → lint-fixer; COVERAGE → coverage-fixer; CI → resume.
    """
    disp = Dispatcher(d.log)

    if d.summary_agent is not None:
        runner = setup.new_runner("automation-agent", d.summary_agent)
        handler = summary_handler(runner)
        disp.register(Kind.CRON_DAILY, handler)
        disp.register(Kind.CRON_WEEKLY, handler)
    if d.lint_kickoff is not None:
        disp.register(Kind.LINT, d.lint_kickoff)
    if d.coverage_kickoff is not None:
        disp.register(Kind.COVERAGE, d.coverage_kickoff)
    if d.ci_resume is not None:
        disp.register(Kind.CI, d.ci_resume)
    return disp


def summary_handler(runner: Runner) -> Handler:
    """Drive the summary workflow runner for a cron envelope, with a fresh session per
    fire."""

    async def handle(e: Envelope) -> None:
        session_id = f"summary-{time.monotonic_ns()}"
        await setup.drive(runner, "system", session_id, "Run the daily commit digest.")

    return handle
