"""The automation-agent service entrypoint.

Wires configuration, tooling, agents, the scheduler, and the webhook server together,
then runs until interrupted. Composition only — logic lives in ``automation_agent/``.

Run with ``python cmd/agent/main.py`` (it is intentionally NOT an importable package:
a top-level ``cmd`` package would shadow the stdlib ``cmd`` module).
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from datetime import timedelta

import uvicorn
from dotenv import load_dotenv
from google.adk.agents import BaseAgent
from google.adk.models import BaseLlm

from automation_agent.agent import covfixer, lintfixer, root, summary
from automation_agent.agent import setup as agent_setup
from automation_agent.agent.fixflow import Deps as FixDeps
from automation_agent.agent.fixflow import Engine
from automation_agent.config import Config, NotifyProvider, load
from automation_agent.githubapi import Client
from automation_agent.ingest import Envelope, Kind
from automation_agent.notify import Notifier, new_notifier
from automation_agent.scheduler import Scheduler
from automation_agent.webhook import Server

log = logging.getLogger("automation_agent")


def build_notifier(cfg: Config) -> Notifier | None:
    """Return a Notifier, or None (with a warning) if not configured."""
    try:
        return new_notifier(cfg.notify_provider.value, cfg.slack_webhook_url, cfg.teams_webhook_url)
    except Exception as exc:
        log.warning("notifier not configured; summary disabled and lint-fixer won't post: %s", exc)
        return None


def build_summary_agent(
    cfg: Config,
    llm: BaseLlm,
    gh: summary.CommitLister,
    notifier: Notifier | None,
    window: timedelta,
    title: str,
) -> BaseAgent | None:
    """Return a summary workflow agent for the given window/title, or None if it can't be
    fully configured."""
    if not cfg.repos:
        log.warning("no REPOS configured; summary workflow disabled")
        return None
    if notifier is None:
        return None  # build_notifier already warned
    try:
        return summary.build_summary_agent(
            summary.Deps(
                llm=llm, gh=gh, notify=notifier, repos=cfg.repos, window=window, title=title
            )
        )
    except Exception as exc:
        log.warning("summary workflow disabled: %s", exc)
        return None


def _payload_handler(
    engine_kickoff: Callable[[bytes], Awaitable[None]],
) -> root.Handler:
    """Adapt a raw-payload kickoff/resume coroutine to a root.Handler."""

    async def handle(e: Envelope) -> None:
        await engine_kickoff(e.payload)

    return handle


def _ci_resume_handler(engines: list[Engine]) -> root.Handler:
    """Hand a check_run event to every engine; each no-ops unless its check matches."""

    async def handle(e: Envelope) -> None:
        errors: list[Exception] = []
        for eng in engines:
            try:
                await eng.resume(e.payload)
            except Exception as exc:  # noqa: BLE001 - collect & continue
                errors.append(exc)
        if errors:
            raise ExceptionGroup("ci resume failed", errors)

    return handle


async def run() -> None:
    load_dotenv()  # load .env if present; real environment still wins
    cfg = load()

    llm = agent_setup.build_llm(cfg)
    code_llm = agent_setup.build_code_llm(cfg)
    gh = Client(cfg.github_token)
    notifier = build_notifier(cfg)

    # One session service + park store, shared by both fix engines (namespaced by app name).
    # memory (default) keeps today's behavior; durable backends persist parked runs across
    # restarts.
    session_service = agent_setup.new_session_service(cfg)
    park_store = agent_setup.new_park_store(cfg)

    # Daily and weekly are distinct agents so the weekly cron posts a real 7-day digest,
    # not a copy of the daily one.
    summary_daily = build_summary_agent(
        cfg, llm, gh, notifier, timedelta(hours=24), "Daily commit digest"
    )
    summary_weekly = build_summary_agent(
        cfg, llm, gh, notifier, timedelta(days=7), "Weekly commit digest"
    )

    # Fix engines (event-driven; work without a notifier — they just won't post results).
    fix_deps = FixDeps(
        llm=llm,
        code_llm=code_llm,
        gh=gh,
        notify=notifier,
        token=cfg.github_token,
        max_iter=cfg.max_iterations,
        ci_timeout=cfg.ci_timeout,
        repos=cfg.repos,
        log=log,
        session_service=session_service,
        park_store=park_store,
    )
    lint_engine = lintfixer.new_engine(fix_deps)
    cov_engine = covfixer.new_engine(fix_deps)
    engines = [lint_engine, cov_engine]

    dispatcher = root.build_root_dispatcher(
        root.Deps(
            summary_daily=summary_daily,
            summary_weekly=summary_weekly,
            lint_kickoff=_payload_handler(lint_engine.kickoff),
            coverage_kickoff=_payload_handler(cov_engine.kickoff),
            ci_resume=_ci_resume_handler(engines),
            log=log,
        )
    )

    loop = asyncio.get_running_loop()

    # In-flight webhook-dispatch tasks. CPython holds only a weak reference to a bare
    # task created by ``loop.create_task``, so a fire-and-forget task can be garbage-
    # collected mid-flight ("Task was destroyed but it is pending!"). Keeping a strong
    # reference here both prevents that and lets the shutdown path drain outstanding work
    # instead of dropping it.
    pending: set[asyncio.Task[None]] = set()

    async def _safe_dispatch(e: Envelope) -> None:
        try:
            await dispatcher.dispatch(e)
        except Exception as exc:  # noqa: BLE001
            log.error("dispatch failed: kind=%s err=%s", e.kind, exc)

    # Scheduler: cron fires on a background thread → marshal the coroutine onto the loop.
    # run_coroutine_threadsafe keeps the coroutine alive via the returned future, so this
    # path is not a GC hazard.
    def _emit(e: Envelope) -> None:
        asyncio.run_coroutine_threadsafe(_safe_dispatch(e), loop)

    sched = Scheduler(_emit)
    sched.add(cfg.cron_daily, Kind.CRON_DAILY)
    sched.add(cfg.cron_weekly, Kind.CRON_WEEKLY)

    # Webhooks enqueue asynchronously and return fast.
    async def _ingest(e: Envelope) -> None:
        task = loop.create_task(_safe_dispatch(e))
        pending.add(task)
        task.add_done_callback(pending.discard)

    # The durable timeout catch-all behind POST /internal/sweep: resolve every engine's
    # parked runs whose CI never reported (Cloud Scheduler drives it on a schedule).
    async def _sweep() -> None:
        for eng in engines:
            await eng.sweep_timeouts()

    if not cfg.github_webhook_secret:
        log.warning(
            "GITHUB_WEBHOOK_SECRET is unset — webhook signatures are NOT verified; "
            "the /webhooks/github route accepts unauthenticated requests (dev only)"
        )
    srv = Server(
        _ingest,
        secret=cfg.github_webhook_secret,
        internal_token=cfg.internal_token,
        sweep=_sweep,
    )

    server = uvicorn.Server(
        uvicorn.Config(srv.app, host="0.0.0.0", port=int(cfg.port), log_level="info")
    )

    sched.start()
    log.info(
        "automation-agent listening: port=%s llm_provider=%s repos=%d notify=%s summary_enabled=%s",
        cfg.port,
        cfg.llm_provider.value,
        len(cfg.repos),
        cfg.notify_provider.value,
        summary_daily is not None or summary_weekly is not None,
    )
    try:
        await server.serve()
    finally:
        log.info("shutting down")
        sched.stop()
        # Drain in-flight webhook dispatches so a clean SIGTERM finishes outstanding
        # work instead of dropping it. Bounded so a stuck dispatch can't hang exit.
        if pending:
            log.info("draining %d in-flight dispatch(es)", len(pending))
            try:
                await asyncio.wait_for(asyncio.gather(*pending, return_exceptions=True), timeout=30)
            except TimeoutError:
                log.warning("drain timed out; %d dispatch(es) abandoned", len(pending))
        # Release a durable park store's backing connection (no-op for the memory backend).
        await park_store.close()


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    try:
        asyncio.run(run())
    except (KeyboardInterrupt, SystemExit):
        pass


if __name__ == "__main__":
    main()
