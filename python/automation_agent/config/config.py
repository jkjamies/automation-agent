"""Runtime configuration for automation-agent, loaded from the environment.

This module is the single source of truth for settings; no other module should
read ``os.environ`` directly. See ``.agents/standards/architecture-design.md`` §12.
"""

from __future__ import annotations

import os
from collections.abc import Callable
from dataclasses import dataclass, field, fields
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


class TasksBackend(StrEnum):
    """Selects the webhook execution transport: how an enqueued envelope reaches the
    dispatcher. See ``specs/20260626-workflow-execution-transport.md``."""

    # Runs each dispatch in a background asyncio task pool (the pre-transport behavior). The
    # default — selecting it changes nothing. Local dev only: it does not survive an instance
    # being reclaimed mid-run, and on Cloud Run the compute is throttled once the response is
    # sent.
    INPROCESS = "inprocess"
    # Enqueues each envelope as a Cloud Tasks HTTP-target task pointed at /internal/dispatch,
    # which executes it in-request (CPU stays allocated) with durable retry + queue rate
    # limiting. The production backend.
    CLOUDTASKS = "cloudtasks"


# Trace exporter values for otel_traces_exporter. The app speaks exactly these four sinks and
# never names a vendor; any OTLP-native backend is otlp + an endpoint. See obs and
# .agents/standards/observability.md.
OTEL_EXPORTER_NONE = "none"
OTEL_EXPORTER_CONSOLE = "console"
OTEL_EXPORTER_OTLP = "otlp"
OTEL_EXPORTER_GCP = "gcp"

# Fields whose value is a credential and must never appear in repr/logs.
_SECRET_FIELDS = frozenset(
    {
        "github_token",
        "github_webhook_secret",
        "internal_token",
        "slack_webhook_url",
        "teams_webhook_url",
        "github_app_private_key_pem",
        # OTLP headers commonly carry a vendor API key (OTEL_EXPORTER_OTLP_HEADERS).
        "otel_exporter_otlp_headers",
    }
)


@dataclass(repr=False)
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
    # GitHub App (production auth). ``github_app_id == 0`` means App mode is off and the
    # static ``github_token`` (PAT) is used. See :meth:`app_mode` and
    # ``specs/20260625-github-app-authentication.md``. ``github_app_private_key_pem`` holds
    # the literal PEM bytes (from the env var or the key file), unescaped and validated to
    # parse as RSA at load time.
    github_app_id: int = 0
    github_app_installation_id: int = 0
    github_app_private_key_pem: bytes = b""
    # git_transport selects the git clone/push transport: "https" (default — uses
    # github_token) or "ssh" (local dev — ssh-agent/keys). SSH only covers the git
    # transport; the GitHub REST API (open/label PR, read CI) still needs a token, so an
    # ssh run without a token warns at startup.
    git_transport: str = "https"
    # git_ssh_key is an explicit private-key path for git_transport=ssh (GIT_SSH_KEY); empty
    # falls back to ssh-agent then the default identity files.
    git_ssh_key: str = ""

    # Notifications
    notify_provider: NotifyProvider = NotifyProvider.SLACK
    slack_webhook_url: str = ""
    teams_webhook_url: str = ""

    # Server
    port: str = "8080"

    # Lint-fixer
    max_iterations: int = 3
    # ci_timeout bounds how long a suspended fix run waits for its CI result before
    # it is resumed with a timeout outcome (notify + stop). Per-run timer, not a scan.
    ci_timeout: timedelta = timedelta(minutes=90)
    github_webhook_secret: str = ""
    # internal_token is the Bearer token guarding the /internal/* endpoints (Cloud Scheduler
    # cron + sweep). Empty disables those endpoints (404).
    internal_token: str = ""
    # agent_pr_label is the single human-facing label applied to every agent PR on creation
    # (AGENT_PR_LABEL). Write-only: PR lookup is by branch, so the label never gates behavior.
    agent_pr_label: str = "automation-agent"

    # Execution transport (webhook -> dispatcher). tasks_backend selects in-process (default)
    # or Cloud Tasks. The Cloud Tasks settings locate the queue and the worker endpoint; the
    # task carries internal_token as its Bearer credential (no new auth var). See
    # specs/20260626-workflow-execution-transport.md.
    tasks_backend: TasksBackend = TasksBackend.INPROCESS
    # tasks_project is the GCP project owning the queue (TASKS_PROJECT); empty falls back to
    # GOOGLE_CLOUD_PROJECT. Required for cloudtasks.
    tasks_project: str = ""
    # tasks_location is the queue's region (TASKS_LOCATION, e.g. "us-central1"). Required for
    # cloudtasks.
    tasks_location: str = ""
    # tasks_queue is the Cloud Tasks queue name (TASKS_QUEUE). Required for cloudtasks.
    tasks_queue: str = ""
    # dispatch_url is the full URL of the /internal/dispatch worker the queue POSTs to
    # (DISPATCH_URL, e.g. https://agent-xyz.run.app/internal/dispatch). Required for cloudtasks.
    dispatch_url: str = ""
    # tasks_dispatch_deadline is how long Cloud Tasks waits for an /internal/dispatch attempt
    # before cancelling it and retrying (TASKS_DISPATCH_DEADLINE). It must be set explicitly on
    # the task: the HTTP-target default is only 10m, so a longer workflow would be cancelled
    # mid-run and retried, duplicating side effects. Cloud Tasks caps this at 30m, which is
    # therefore the default and the ceiling. Used only in cloudtasks mode.
    tasks_dispatch_deadline: timedelta = timedelta(minutes=30)

    # Observability (OpenTelemetry tracing). All off by default: with
    # otel_traces_exporter=none nothing is registered and the service is unchanged. The agent
    # framework already emits a native span tree; setting an exporter turns it on. Owned here
    # (the single place that reads OTEL_*) and handed to obs as a typed struct. See obs and
    # .agents/standards/observability.md.
    # otel_traces_exporter selects the sink: none | console | otlp | gcp.
    otel_traces_exporter: str = OTEL_EXPORTER_NONE
    # otel_traces_exporter_set records whether OTEL_TRACES_EXPORTER was explicitly provided in
    # the environment (vs. defaulted to none). The playground uses this to default to the
    # console exporter unless an operator opted into a specific sink — deriving that decision
    # from loaded config rather than reading os.environ itself (config is the only env reader).
    otel_traces_exporter_set: bool = False
    # otel_service_name is the resource service.name on every span (OTEL_SERVICE_NAME).
    otel_service_name: str = "automation-agent"
    # otel_exporter_otlp_endpoint / otel_exporter_otlp_headers configure the otlp exporter
    # (standard OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_HEADERS). The endpoint is
    # required for the otlp exporter; the headers are a secret (masked in repr).
    otel_exporter_otlp_endpoint: str = ""
    otel_exporter_otlp_headers: str = ""
    # otel_traces_sampler is a standard OTEL_TRACES_SAMPLER value; the default
    # parentbased_always_on is correct here (trace volume is one-per-webhook).
    otel_traces_sampler: str = "parentbased_always_on"
    # otel_capture_message_content gates whether prompt/response bodies are captured as span
    # attributes (sensitive: they are reviewed source code). Off by default. The agent
    # framework reads this standard GenAI-semconv var natively —
    # OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT — so surfacing it here keeps it under
    # the single config source. Model/token/tool/latency attributes are captured regardless.
    otel_capture_message_content: bool = False

    def app_mode(self) -> bool:
        """Whether GitHub App authentication is configured (production path). False means
        the static PAT fallback (``github_token``) is used."""
        return self.github_app_id != 0

    def __repr__(self) -> str:
        """Render the config with every credential masked, so a debug/startup log never
        leaks a secret. The dataclass-synthesized repr would otherwise dump the App
        private key, PAT, webhook secret, internal token, and webhook URLs verbatim; an
        unset secret stays visibly empty, a set one collapses to ``***``. Mirrors Go's
        ``String()``, JS's inspect redaction, and Kotlin's ``toString``."""
        parts = []
        for f in fields(self):
            value = getattr(self, f.name)
            if f.name in _SECRET_FIELDS and value:
                value = "***"
            parts.append(f"{f.name}={value!r}")
        return f"{type(self).__name__}({', '.join(parts)})"

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
        if self.git_transport not in ("https", "ssh"):
            raise ValueError(
                f"invalid GIT_TRANSPORT {self.git_transport!r} (want https|ssh)"
            )
        if self.tasks_backend == TasksBackend.CLOUDTASKS:
            self._validate_cloudtasks()
        elif self.tasks_backend != TasksBackend.INPROCESS:
            raise ValueError(
                f"invalid TASKS_BACKEND {self.tasks_backend!r} (want inprocess|cloudtasks)"
            )
        if self.max_iterations < 1:
            raise ValueError(f"MAX_ITERATIONS must be >= 1, got {self.max_iterations}")
        try:
            port = int(self.port)
        except ValueError as exc:
            raise ValueError(f"PORT must be numeric, got {self.port!r}") from exc
        if not (0 < port < 65536):
            raise ValueError(f"PORT must be in 1..65535, got {port}")
        # In App mode an installation can see every repo it is installed on, so an empty
        # allow-list ("act on all repos", the PAT-mode default) is a footgun — fail fast.
        # PAT mode keeps "empty = all" for local-dev back-compat.
        if self.app_mode() and not self.repos:
            raise ValueError(
                "REPOS must be set in GitHub App mode (empty REPOS = all repos is "
                "rejected to avoid acting on every installed repo)"
            )
        self._validate_observability()

    def _validate_observability(self) -> None:
        """Check the OTEL_* settings: the exporter must be one of the four known sinks, and
        the otlp exporter needs an endpoint (else it would silently export nowhere).

        Raises:
            ValueError: on an unknown exporter or otlp with no endpoint.
        """
        if self.otel_traces_exporter in (
            OTEL_EXPORTER_NONE,
            OTEL_EXPORTER_CONSOLE,
            OTEL_EXPORTER_GCP,
        ):
            return
        if self.otel_traces_exporter == OTEL_EXPORTER_OTLP:
            if not self.otel_exporter_otlp_endpoint.strip():
                raise ValueError(
                    "OTEL_TRACES_EXPORTER=otlp requires OTEL_EXPORTER_OTLP_ENDPOINT"
                )
            return
        raise ValueError(
            f"invalid OTEL_TRACES_EXPORTER {self.otel_traces_exporter!r} "
            "(want none|console|otlp|gcp)"
        )

    def _validate_cloudtasks(self) -> None:
        """Check the cloudtasks-backend requirements: the queue coordinates and worker URL,
        plus the Bearer token the task carries. Without INTERNAL_TOKEN, /internal/dispatch is
        disabled (404) and every task would fail permanently — so fail fast at startup rather
        than silently never dispatching.

        Raises:
            ValueError: listing every missing/invalid cloudtasks setting.
        """
        from urllib.parse import urlparse

        missing: list[str] = []
        if not self.tasks_project:
            missing.append("TASKS_PROJECT (or GOOGLE_CLOUD_PROJECT)")
        if not self.tasks_location:
            missing.append("TASKS_LOCATION")
        if not self.tasks_queue:
            missing.append("TASKS_QUEUE")
        # DISPATCH_URL must be an absolute https URL: the Cloud Tasks task carries
        # INTERNAL_TOKEN as a Bearer header to it, so a plaintext http:// target would leak
        # the token in transit (same posture as gitrepo refusing a token over http://). It
        # must also resolve to the /internal/dispatch worker route — a base URL or a stray
        # path would pass the scheme check and then 404 every task at runtime. A suffix match
        # (not equality) tolerates a gateway path prefix while still requiring the path.
        if not self.dispatch_url:
            missing.append("DISPATCH_URL")
        else:
            parsed = urlparse(self.dispatch_url)
            if (
                parsed.scheme != "https"
                or not parsed.netloc
                or not parsed.path.endswith("/internal/dispatch")
            ):
                missing.append(
                    "DISPATCH_URL (must be an absolute https:// URL ending in /internal/dispatch)"
                )
        if not self.internal_token:
            missing.append("INTERNAL_TOKEN (the Bearer the task carries to /internal/dispatch)")
        # Cloud Tasks clamps an HTTP-target dispatch deadline to 15s..30m; a value outside
        # that range is silently rejected at create time, so reject it here instead.
        if not (timedelta(seconds=15) <= self.tasks_dispatch_deadline <= timedelta(minutes=30)):
            missing.append("TASKS_DISPATCH_DEADLINE (must be between 15s and 30m)")
        # In Cloud Tasks mode the deployment is production-facing, so an unverified webhook
        # surface is a real exposure rather than a dev convenience — require the HMAC secret
        # (it stays an opt-in warning only for the local inprocess default).
        if not self.github_webhook_secret:
            missing.append(
                "GITHUB_WEBHOOK_SECRET (webhook signatures must be verified in production)"
            )
        if missing:
            raise ValueError("TASKS_BACKEND=cloudtasks requires " + ", ".join(missing))


def load() -> Config:
    """Read configuration from the process environment, applying defaults."""
    cfg = load_from(os.environ.get)
    # When neither GITHUB_TOKEN nor GH_TOKEN is set, fall back to the developer's gh
    # CLI login so a local run authenticates to GitHub without a hand-set token.
    # Skipped in App mode: the App provider mints its own tokens, so shelling out to gh
    # would be a useless subprocess that could also hydrate a PAT never asked for.
    if not cfg.app_mode() and not cfg.github_token:
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
        git_transport=_get_or(get, "GIT_TRANSPORT", "https"),
        git_ssh_key=_get_or(get, "GIT_SSH_KEY", ""),
        notify_provider=NotifyProvider(
            _get_or(get, "NOTIFY_PROVIDER", NotifyProvider.SLACK.value)
        ),
        slack_webhook_url=_get_or(get, "SLACK_WEBHOOK_URL", ""),
        teams_webhook_url=_get_or(get, "TEAMS_WEBHOOK_URL", ""),
        port=_get_or(get, "PORT", "8080"),
        max_iterations=max_iterations,
        ci_timeout=_parse_duration(_get_or(get, "CI_TIMEOUT", "90m")),
        github_webhook_secret=_get_or(get, "GITHUB_WEBHOOK_SECRET", ""),
        internal_token=_get_or(get, "INTERNAL_TOKEN", ""),
        agent_pr_label=_get_or(get, "AGENT_PR_LABEL", "automation-agent"),
        tasks_backend=TasksBackend(_get_or(get, "TASKS_BACKEND", TasksBackend.INPROCESS.value)),
        tasks_project=_get_or(get, "TASKS_PROJECT", _get_or(get, "GOOGLE_CLOUD_PROJECT", "")),
        tasks_location=_get_or(get, "TASKS_LOCATION", ""),
        tasks_queue=_get_or(get, "TASKS_QUEUE", ""),
        dispatch_url=_get_or(get, "DISPATCH_URL", ""),
        tasks_dispatch_deadline=_parse_duration(
            _get_or(get, "TASKS_DISPATCH_DEADLINE", "30m"), "TASKS_DISPATCH_DEADLINE"
        ),
        otel_traces_exporter=_get_or(get, "OTEL_TRACES_EXPORTER", OTEL_EXPORTER_NONE),
        otel_traces_exporter_set=bool((get("OTEL_TRACES_EXPORTER") or "").strip()),
        otel_service_name=_get_or(get, "OTEL_SERVICE_NAME", "automation-agent"),
        otel_exporter_otlp_endpoint=_get_or(get, "OTEL_EXPORTER_OTLP_ENDPOINT", ""),
        otel_exporter_otlp_headers=_get_or(get, "OTEL_EXPORTER_OTLP_HEADERS", ""),
        otel_traces_sampler=_get_or(get, "OTEL_TRACES_SAMPLER", "parentbased_always_on"),
        otel_capture_message_content=_get_bool(
            get, "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", False
        ),
    )

    # Code models default to the base models when unset.
    if cfg.ollama_code_model == "":
        cfg.ollama_code_model = cfg.ollama_model
    if cfg.gemini_code_model == "":
        cfg.gemini_code_model = cfg.gemini_model

    # Resolve GitHub App credentials (production auth path). Absent App vars leave the
    # zero values — PAT mode. Partial/misconfigured App vars are a startup error, never a
    # silent fallback to PAT.
    app_id, install_id, pem = _resolve_github_app(get)
    cfg.github_app_id = app_id
    cfg.github_app_installation_id = install_id
    cfg.github_app_private_key_pem = pem

    cfg.validate()
    return cfg


def _get_or(get: Lookup, key: str, default: str) -> str:
    # Trim so trailing whitespace/newlines on a value from the real environment
    # (e.g. a CI secret with a trailing newline) do not leak into the setting.
    v = get(key)
    if v is not None:
        v = v.strip()
        if v:
            return v
    return default


def _get_bool(get: Lookup, key: str, default: bool) -> bool:
    """Parse a boolean env var. Accepts the common truthy/falsy spellings
    (1/true/yes/on and 0/false/no/off, case-insensitively); an unset or blank value uses
    ``default``. An unrecognized value falls back to ``default`` rather than failing — the
    flag is advisory."""
    v = get(key)
    if v is None:
        return default
    v = v.strip().lower()
    if v == "":
        return default
    if v in ("1", "true", "yes", "on"):
        return True
    if v in ("0", "false", "no", "off"):
        return False
    return default


def _split_list(s: str) -> list[str]:
    if not s.strip():
        return []
    return [t.strip() for t in s.split(",") if t.strip()]


def _resolve_github_app(get: Lookup) -> tuple[int, int, bytes]:
    """Read the ``GITHUB_APP_*`` vars and decide the auth mode, returning
    ``(app_id, installation_id, private_key_pem)``.

    With none set, returns ``(0, 0, b"")`` — PAT mode. With any set, App mode is intended
    and every requirement is enforced (App ID, a pinned installation id, and exactly one
    private-key source), so a partial configuration is a startup error, not a silent
    fallback to PAT.

    Raises:
        ValueError: on a partial/misconfigured App setup, a non-positive id, an
            unreadable key file, or a key that is not valid RSA PEM.
    """
    app_id_str = _get_or(get, "GITHUB_APP_ID", "")
    install_id_str = _get_or(get, "GITHUB_APP_INSTALLATION_ID", "")
    key_literal = _get_or(get, "GITHUB_APP_PRIVATE_KEY", "")
    key_path = _get_or(get, "GITHUB_APP_PRIVATE_KEY_PATH", "")

    if not app_id_str and not install_id_str and not key_literal and not key_path:
        return 0, 0, b""  # PAT mode — no App vars present.

    # Any App var present signals intent to use App mode; require the full set.
    if not app_id_str:
        raise ValueError(
            "GITHUB_APP_* set but GITHUB_APP_ID is missing (App mode requires GITHUB_APP_ID)"
        )
    if not install_id_str:
        raise ValueError(
            "App mode requires GITHUB_APP_INSTALLATION_ID (single pinned installation)"
        )
    if key_literal and key_path:
        raise ValueError(
            "set exactly one of GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH, not both"
        )
    if not key_literal and not key_path:
        raise ValueError(
            "App mode requires one of GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH"
        )

    try:
        app_id = int(app_id_str)
    except ValueError as exc:
        raise ValueError(f"GITHUB_APP_ID must be numeric, got {app_id_str!r}") from exc
    # A non-positive App ID is invalid and, worse, 0 would collide with app_mode()'s
    # zero-value sentinel and silently fall back to PAT — reject it explicitly.
    if app_id <= 0:
        raise ValueError(f"GITHUB_APP_ID must be > 0, got {app_id}")
    try:
        install_id = int(install_id_str)
    except ValueError as exc:
        raise ValueError(
            f"GITHUB_APP_INSTALLATION_ID must be numeric, got {install_id_str!r}"
        ) from exc
    if install_id <= 0:
        raise ValueError(f"GITHUB_APP_INSTALLATION_ID must be > 0, got {install_id}")

    if key_path:
        try:
            with open(key_path, "rb") as f:
                raw = f.read()
        except OSError as exc:
            raise ValueError(
                f"read GITHUB_APP_PRIVATE_KEY_PATH {key_path!r}: {exc}"
            ) from exc
    else:
        raw = key_literal.encode("utf-8")
    return app_id, install_id, _normalize_private_key_pem(raw)


def _normalize_private_key_pem(raw: bytes) -> bytes:
    """Make the App private key robust to how it is delivered: CI secret stores often
    flatten newlines to the literal characters ``\\n``, so when the value looks like PEM
    and contains escaped ``\\n`` sequences, restore them — even if a real trailing newline
    is also present. Then validate the key parses as an RSA private key, failing at
    startup with a clear message rather than a cryptic RS256 error at first token exchange.

    Raises:
        ValueError: if the bytes are not valid PEM or are not an RSA private key.
    """
    # Imported lazily so the common PAT path never pays the cryptography import cost.
    from cryptography.hazmat.primitives.asymmetric import rsa
    from cryptography.hazmat.primitives.serialization import load_pem_private_key

    if b"-----BEGIN" in raw and rb"\n" in raw:
        raw = raw.replace(rb"\n", b"\n")
    try:
        key = load_pem_private_key(raw, password=None)
    except Exception as exc:  # noqa: BLE001
        raise ValueError(
            f"GitHub App private key is not valid PEM / does not parse: {exc}"
        ) from exc
    # GitHub App keys are RSA, and RS256 JWT signing requires an RSA key — reject
    # EC/Ed25519 here rather than failing cryptically at the first token exchange.
    if not isinstance(key, rsa.RSAPrivateKey):
        raise ValueError(
            f"GitHub App private key must be RSA, got {type(key).__name__}"
        )
    return raw


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


def _parse_duration(s: str, name: str = "CI_TIMEOUT") -> timedelta:
    """Parse a duration string (e.g. ``90m``, ``1h30m``) into a timedelta.

    Supports the subset of unit suffixes needed for CI_TIMEOUT / TASKS_DISPATCH_DEADLINE.
    ``name`` is the env var the value came from, used to prefix the error message.

    Raises:
        ValueError: if the string is empty or malformed.
    """
    import re

    text = s.strip()
    if text == "":
        raise ValueError(f"{name}: empty duration")
    matches = re.findall(r"(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)", text)
    if not matches or "".join(n + u for n, u in matches) != text:
        raise ValueError(f"{name}: invalid duration {s!r}")
    seconds = sum(float(num) * _DURATION_UNITS[unit] for num, unit in matches)
    return timedelta(seconds=seconds)
