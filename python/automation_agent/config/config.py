"""Runtime configuration for automation-agent, loaded from the environment.

This module is the single source of truth for settings; no other module should
read ``os.environ`` directly. See ``.agents/standards/architecture-design.md`` §12.
"""

from __future__ import annotations

import os
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import timedelta
from enum import StrEnum

Lookup = Callable[[str], str | None]


class Provider(StrEnum):
    """Selects which LLM backend agents use."""

    OLLAMA = "ollama"
    GEMINI = "gemini"


class NotifyProvider(StrEnum):
    """Selects where summaries are posted."""

    SLACK = "slack"
    TEAMS = "teams"


class SessionBackend(StrEnum):
    """Selects where the ADK session (the durable suspend/resume history of the parked
    fix loop) and its park record are stored."""

    # In-process: tests and ephemeral local runs. A restart strands parked runs. This is
    # the default — selecting it changes nothing.
    MEMORY = "memory"
    # Persists sessions to a local sqlite file (adk SqliteSessionService) so a parked run
    # survives a restart. For real local runs.
    SQLITE = "sqlite"
    # Cloud backend (serverless, scales to zero): adk-python's native Firestore session
    # service (google.adk.integrations.firestore) plus a custom park store on the native
    # google-cloud-firestore client (the park record is our own concept, not an ADK type).
    FIRESTORE = "firestore"


@dataclass
class Config:
    """All runtime settings."""

    # LLM
    llm_provider: Provider = Provider.OLLAMA
    ollama_host: str = "http://localhost:11434"
    ollama_model: str = "gemma4:12b"  # default model: triage, explore, summary
    gemini_model: str = ""
    # Code model: the (typically larger) model used for the code-change steps
    # (lint rewrite, coverage test generation). Falls back to the default model.
    ollama_code_model: str = ""
    gemini_code_model: str = ""

    # Sessions
    session_backend: SessionBackend = SessionBackend.MEMORY
    # sqlite_dsn is the file path for SESSION_BACKEND=sqlite (ignored otherwise). adk's
    # SqliteSessionService takes a plain path (not a SQLAlchemy URL); the park store shares
    # the same file.
    sqlite_dsn: str = "automation-agent.db"
    # firestore_project is the GCP project for SESSION_BACKEND=firestore; empty detects it
    # from ADC / GOOGLE_CLOUD_PROJECT. firestore_collection is the collection-name prefix.
    firestore_project: str = ""
    firestore_collection: str = "automation_agent"

    # GitHub / repos
    repos: list[str] = field(default_factory=list)
    github_token: str = ""

    # Notifications
    notify_provider: NotifyProvider = NotifyProvider.SLACK
    slack_webhook_url: str = ""
    teams_webhook_url: str = ""

    # Server / schedule
    port: str = "8080"
    cron_daily: str = "0 9 * * *"
    cron_weekly: str = "0 9 * * 1"

    # Lint-fixer
    max_iterations: int = 3
    # ci_timeout bounds how long a suspended fix run waits for its CI result before
    # it is resumed with a timeout outcome (notify + stop). Per-run timer, not a scan.
    ci_timeout: timedelta = timedelta(minutes=90)
    github_webhook_secret: str = ""
    # internal_token is the Bearer token guarding the /internal/* endpoints (Cloud Scheduler
    # cron + sweep). Empty disables those endpoints (404).
    internal_token: str = ""
    agent_pr_label: str = "automation-agent"
    agent_check_name: str = "agent-lint-verify"

    def validate(self) -> None:
        """Check invariants that defaults alone cannot guarantee.

        Raises:
            ValueError: if a provider enum is invalid or max_iterations < 1.
        """
        if self.llm_provider not in (Provider.OLLAMA, Provider.GEMINI):
            raise ValueError(
                f"invalid LLM_PROVIDER {self.llm_provider!r} (want ollama|gemini)"
            )
        if self.notify_provider not in (NotifyProvider.SLACK, NotifyProvider.TEAMS):
            raise ValueError(
                f"invalid NOTIFY_PROVIDER {self.notify_provider!r} (want slack|teams)"
            )
        if self.session_backend not in (
            SessionBackend.MEMORY,
            SessionBackend.SQLITE,
            SessionBackend.FIRESTORE,
        ):
            raise ValueError(
                f"invalid SESSION_BACKEND {self.session_backend!r} "
                "(want memory|sqlite|firestore)"
            )
        if self.max_iterations < 1:
            raise ValueError(f"MAX_ITERATIONS must be >= 1, got {self.max_iterations}")
        try:
            port = int(self.port)
        except ValueError as exc:
            raise ValueError(f"PORT must be numeric, got {self.port!r}") from exc
        if not (0 < port < 65536):
            raise ValueError(f"PORT must be in 1..65535, got {port}")


def load() -> Config:
    """Read configuration from the process environment, applying defaults."""
    cfg = load_from(os.environ.get)
    # When neither GITHUB_TOKEN nor GH_TOKEN is set, fall back to the developer's gh
    # CLI login so a local run authenticates to GitHub without a hand-set token.
    if not cfg.github_token:
        cfg.github_token = _gh_cli_token()
    return cfg


def _gh_cli_token() -> str:
    """Return the token from ``gh auth token``, or "" if the gh CLI is missing,
    unauthenticated, or errors.

    This is the one place config shells out rather than reading the environment; it
    exists so local runs reuse an existing gh login. The short timeout guards against
    a hung subprocess stalling startup.
    """
    import shutil
    import subprocess

    if shutil.which("gh") is None:
        return ""
    try:
        proc = subprocess.run(
            ["gh", "auth", "token"],
            capture_output=True,
            text=True,
            timeout=5,
            check=True,
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    return proc.stdout.strip()


def load_from(get: Lookup) -> Config:
    """Build a Config from an arbitrary lookup func.

    This keeps :func:`load` testable without mutating the real environment.

    Raises:
        ValueError: on an unparseable MAX_ITERATIONS / CI_TIMEOUT or a failed
            :meth:`Config.validate`.
    """
    try:
        max_iterations = int(_get_or(get, "MAX_ITERATIONS", "3"))
    except ValueError as exc:
        raise ValueError(f"MAX_ITERATIONS: {exc}") from exc

    cfg = Config(
        llm_provider=Provider(_get_or(get, "LLM_PROVIDER", Provider.OLLAMA.value)),
        ollama_host=_get_or(get, "OLLAMA_HOST", "http://localhost:11434"),
        ollama_model=_get_or(get, "OLLAMA_MODEL", "gemma4:12b"),
        ollama_code_model=_get_or(get, "OLLAMA_CODE_MODEL", "gemma4:26b"),
        gemini_model=_get_or(get, "GEMINI_MODEL", ""),
        gemini_code_model=_get_or(get, "GEMINI_CODE_MODEL", ""),
        session_backend=SessionBackend(
            _get_or(get, "SESSION_BACKEND", SessionBackend.MEMORY.value)
        ),
        sqlite_dsn=_get_or(get, "SQLITE_DSN", "automation-agent.db"),
        firestore_project=_get_or(get, "FIRESTORE_PROJECT", ""),
        firestore_collection=_get_or(get, "FIRESTORE_COLLECTION", "automation_agent"),
        repos=_split_list(_get_or(get, "REPOS", "")),
        github_token=_get_or(get, "GITHUB_TOKEN", _get_or(get, "GH_TOKEN", "")),
        notify_provider=NotifyProvider(
            _get_or(get, "NOTIFY_PROVIDER", NotifyProvider.SLACK.value)
        ),
        slack_webhook_url=_get_or(get, "SLACK_WEBHOOK_URL", ""),
        teams_webhook_url=_get_or(get, "TEAMS_WEBHOOK_URL", ""),
        port=_get_or(get, "PORT", "8080"),
        cron_daily=_get_or(get, "CRON_DAILY", "0 9 * * *"),
        cron_weekly=_get_or(get, "CRON_WEEKLY", "0 9 * * 1"),
        max_iterations=max_iterations,
        ci_timeout=_parse_duration(_get_or(get, "CI_TIMEOUT", "90m")),
        github_webhook_secret=_get_or(get, "GITHUB_WEBHOOK_SECRET", ""),
        internal_token=_get_or(get, "INTERNAL_TOKEN", ""),
        agent_pr_label=_get_or(get, "AGENT_PR_LABEL", "automation-agent"),
        agent_check_name=_get_or(get, "AGENT_CHECK_NAME", "agent-lint-verify"),
    )

    # Code models default to the base models when unset.
    if cfg.ollama_code_model == "":
        cfg.ollama_code_model = cfg.ollama_model
    if cfg.gemini_code_model == "":
        cfg.gemini_code_model = cfg.gemini_model

    cfg.validate()
    return cfg


def _get_or(get: Lookup, key: str, default: str) -> str:
    v = get(key)
    if v:
        return v
    return default


def _split_list(s: str) -> list[str]:
    if not s.strip():
        return []
    return [t.strip() for t in s.split(",") if t.strip()]


# Duration unit table (subset that matters for CI_TIMEOUT).
_DURATION_UNITS: dict[str, float] = {
    "ns": 1e-9,
    "us": 1e-6,
    "µs": 1e-6,
    "ms": 1e-3,
    "s": 1.0,
    "m": 60.0,
    "h": 3600.0,
}


def _parse_duration(s: str) -> timedelta:
    """Parse a duration string (e.g. ``90m``, ``1h30m``) into a timedelta.

    Supports the subset of unit suffixes needed for CI_TIMEOUT.

    Raises:
        ValueError: if the string is empty or malformed.
    """
    import re

    text = s.strip()
    if text == "":
        raise ValueError("CI_TIMEOUT: empty duration")
    matches = re.findall(r"(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)", text)
    if not matches or "".join(n + u for n, u in matches) != text:
        raise ValueError(f"CI_TIMEOUT: invalid duration {s!r}")
    seconds = sum(float(num) * _DURATION_UNITS[unit] for num, unit in matches)
    return timedelta(seconds=seconds)
