"""The automation-agent service entrypoint.

Wires configuration, tooling, agents, and the webhook server together, then runs until
interrupted. Composition only — logic lives in ``automation_agent/``.

Run with ``python cmd/agent/main.py`` (it is intentionally NOT an importable package:
a top-level ``cmd`` package would shadow the stdlib ``cmd`` module).
"""

from __future__ import annotations

import asyncio
import contextlib
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
from automation_agent.auth import StaticProvider, TokenProvider, new_app_provider
from automation_agent.config import Config, TasksBackend, load
from automation_agent.githubapi import Client
from automation_agent.ingest import Envelope
from automation_agent.notify import Notifier, new_notifier
from automation_agent.obs import Config as ObsConfig
from automation_agent.obs import TracingMiddleware, install_log_correlation
from automation_agent.obs import init as init_tracing
from automation_agent.tasks import DispatchFunc, InProcess, Transport, new_cloud_tasks
from automation_agent.webhook import Server

log = logging.getLogger("automation_agent")


def build_transport(cfg: Config, dispatch: DispatchFunc) -> Transport:
    """Select the webhook execution transport: Cloud Tasks in production (durable,
    in-request, rate-limited by the queue) or the in-process task pool for local dev (the
    default). See ``specs/20260626-workflow-execution-transport.md``."""
    if cfg.tasks_backend == TasksBackend.CLOUDTASKS:
        log.info(
            "execution transport: cloud tasks project=%s location=%s queue=%s dispatch_url=%s",
            cfg.tasks_project,
            cfg.tasks_location,
            cfg.tasks_queue,
            cfg.dispatch_url,
        )
        return new_cloud_tasks(
            cfg.tasks_project,
            cfg.tasks_location,
            cfg.tasks_queue,
            cfg.dispatch_url,
            cfg.internal_token,
            cfg.tasks_dispatch_deadline,
        )
    log.info("execution transport: in-process (local/default)")
    return InProcess(dispatch, log)


def build_token_provider(cfg: Config) -> TokenProvider:
    """Select the GitHub auth provider: App installation tokens in production (when the
    GITHUB_APP_* vars are set), else the static PAT/anonymous fallback for local dev. One
    provider authenticates both the REST client and git transport."""
    if cfg.app_mode():
        return new_app_provider(
            cfg.github_app_id,
            cfg.github_app_installation_id,
            cfg.github_app_private_key_pem,
        )
    return StaticProvider(cfg.github_token)


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

    # Register the OTel tracer provider so the agent framework's native span tree is exported.
    # No-op by default (OTEL_TRACES_EXPORTER=none) — merging changes nothing in prod until an
    # environment opts in. shutdown_tracing force-flushes buffered spans at exit (the
    # scale-to-zero span-loss guard, mirrored per-request by the HTTP middleware). Correlation
    # then stamps trace_id/span_id onto every log record emitted under a span.
    shutdown_tracing = init_tracing(
        ObsConfig(
            exporter=cfg.otel_traces_exporter,
            service_name=cfg.otel_service_name,
            otlp_endpoint=cfg.otel_exporter_otlp_endpoint,
            otlp_headers=cfg.otel_exporter_otlp_headers,
            sampler=cfg.otel_traces_sampler,
        )
    )
    install_log_correlation()

    llm = agent_setup.build_llm(cfg)
    code_llm = agent_setup.build_code_llm(cfg)
    provider = build_token_provider(cfg)
    gh = Client(provider)
    # SSH only authenticates the git transport (clone/push). In PAT mode the GitHub REST
    # API — opening and labeling PRs, reading the CI check — still needs a token (or `gh`
    # login). Warn rather than fail so read-only/dry-run flows still work, but PR
    # operations will not. App mode authenticates the REST API with the App token
    # regardless of git transport, so the warning does not apply there.
    if cfg.git_transport == "ssh" and not cfg.github_token and not cfg.app_mode():
        log.warning(
            "GIT_TRANSPORT=ssh but no GitHub token found (GITHUB_TOKEN/GH_TOKEN/`gh auth "
            "token`); git clone+push will use ssh, but PR operations against the REST API "
            "will fail — run `gh auth login` or set a token"
        )
    notifier = build_notifier(cfg)

    # One session service + park store, shared by both fix engines (namespaced by app name).
    # memory (default) keeps today's behavior; durable backends persist parked runs across
    # restarts.
    session_service = agent_setup.new_session_service(cfg)
    park_store = agent_setup.new_park_store(cfg)

    # The daily Cloud Scheduler trigger fires this summary agent.
    summary_daily = build_summary_agent(
        cfg, llm, gh, notifier, timedelta(hours=24), "Daily commit digest"
    )
    # /internal/cron/daily is the only daily-digest trigger, and it 404s when INTERNAL_TOKEN
    # is unset. Warn rather than fail silently so a built-but-unreachable digest is visible.
    if summary_daily is not None and not cfg.internal_token:
        log.warning(
            "daily summary built but INTERNAL_TOKEN is unset; /internal/cron/daily is "
            "disabled (404), so the digest cannot be triggered",
        )

    # Fix engines (event-driven; work without a notifier — they just won't post results).
    fix_deps = FixDeps(
        llm=llm,
        code_llm=code_llm,
        gh=gh,
        notify=notifier,
        provider=provider,
        git_transport=cfg.git_transport,
        ssh_key=cfg.git_ssh_key,
        pr_label=cfg.agent_pr_label,
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
            lint_kickoff=_payload_handler(lint_engine.kickoff),
            coverage_kickoff=_payload_handler(cov_engine.kickoff),
            ci_resume=_ci_resume_handler(engines),
            log=log,
        )
    )

    # Webhooks enqueue asynchronously and return fast. The transport runs the dispatch:
    # in-process (default) on a bounded task pool drained on SIGTERM, or — in production — via
    # Cloud Tasks, which delivers each envelope to /internal/dispatch so the compute runs
    # in-request (CPU stays allocated) with durable retry. See
    # specs/20260626-workflow-execution-transport.md.
    transport = build_transport(cfg, dispatcher.dispatch)

    async def _ingest(e: Envelope) -> None:
        await transport.enqueue(e)

    # The durable timeout catch-all behind POST /internal/sweep: resolve every engine's
    # parked runs whose CI never reported (Cloud Scheduler drives it on a schedule). One
    # engine's failure must not stop the others — a stuck run on another engine still needs
    # freeing — so collect-and-continue (like _ci_resume_handler), then surface so the
    # handler 500s and Cloud Scheduler retries.
    async def _sweep() -> None:
        errors: list[Exception] = []
        for eng in engines:
            try:
                await eng.sweep_timeouts()
            except Exception as exc:  # noqa: BLE001 - collect & continue
                log.error("sweep failed for an engine: %s", exc)
                errors.append(exc)
        if errors:
            raise ExceptionGroup("sweep failed", errors)

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
        # /internal/dispatch executes a queued envelope in-request (the Cloud Tasks worker).
        dispatch=dispatcher.dispatch,
        log=log,
    )

    # TracingMiddleware adds a server span per request (the trace root on ingress, continued
    # from the task's traceparent header on /internal/dispatch) and force-flushes spans before
    # each response returns. No-op when tracing is disabled, so the wrapped app is unchanged.
    app = TracingMiddleware(srv.app)
    server = uvicorn.Server(
        uvicorn.Config(app, host="0.0.0.0", port=int(cfg.port), log_level="info")
    )

    log.info(
        "automation-agent listening: port=%s llm_provider=%s repos=%d notify=%s summary_enabled=%s",
        cfg.port,
        cfg.llm_provider.value,
        len(cfg.repos),
        cfg.notify_provider.value,
        summary_daily is not None,
    )
    try:
        await server.serve()
    finally:
        log.info("shutting down")
        # Close the transport after the server stops accepting: the in-process backend drains
        # in-flight dispatches (bounded), the Cloud Tasks backend closes its client. Done
        # before the park-store close so any draining dispatch still has its store.
        await transport.close()
        # Release a durable park store's backing connection (no-op for the memory backend).
        await park_store.close()
        # Force-flush and release the tracer provider last, so spans from the draining
        # dispatches and shutdown path are exported before the process exits (no-op when
        # tracing is disabled).
        shutdown_tracing()


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    with contextlib.suppress(KeyboardInterrupt, SystemExit):
        asyncio.run(run())


if __name__ == "__main__":
    main()
