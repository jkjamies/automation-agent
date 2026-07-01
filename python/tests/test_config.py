"""Tests for config loading."""

from __future__ import annotations

import pytest

from automation_agent.config import (
    NotifyProvider,
    Provider,
    SessionBackend,
    TasksBackend,
    load_from,
)


def map_lookup(m: dict[str, str]):
    return m.get


def full_cloudtasks_env() -> dict[str, str]:
    """A complete, valid cloudtasks configuration (the base for negative cases)."""
    return {
        "TASKS_BACKEND": "cloudtasks",
        "TASKS_PROJECT": "proj",
        "TASKS_LOCATION": "us-central1",
        "TASKS_QUEUE": "agent-q",
        "DISPATCH_URL": "https://svc.run.app/internal/dispatch",
        "INTERNAL_TOKEN": "sekret",
        "GITHUB_WEBHOOK_SECRET": "hmac",
    }


def test_load_defaults() -> None:
    c = load_from(map_lookup({}))
    assert c.llm_provider == Provider.OLLAMA
    assert c.ollama_model == "gemma4:12b"
    assert c.ollama_code_model == "gemma4:26b"  # default
    assert c.notify_provider == NotifyProvider.SLACK
    assert c.max_iterations == 3
    assert c.ci_timeout.total_seconds() == 90 * 60
    assert c.agent_pr_label == "automation-agent"
    # Sessions default to in-process (memory); a restart strands parked runs.
    assert c.session_backend == SessionBackend.MEMORY
    assert c.sqlite_dsn == "automation-agent.db"
    assert c.firestore_project == ""
    assert c.firestore_collection == "automation_agent"
    assert c.internal_token == ""
    # Git transport defaults to https (token / GitHub App); ssh is opt-in for local dev.
    assert c.git_transport == "https"
    assert c.git_ssh_key == ""


def test_session_backend_override() -> None:
    c = load_from(
        map_lookup(
            {
                "SESSION_BACKEND": "sqlite",
                "SQLITE_DSN": "/tmp/runs.db",
                "FIRESTORE_PROJECT": "my-proj",
                "FIRESTORE_COLLECTION": "agent_runs",
                "INTERNAL_TOKEN": "s3cret",
            }
        )
    )
    assert c.session_backend == SessionBackend.SQLITE
    assert c.sqlite_dsn == "/tmp/runs.db"
    assert c.firestore_project == "my-proj"
    assert c.firestore_collection == "agent_runs"
    assert c.internal_token == "s3cret"


def test_invalid_session_backend() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"SESSION_BACKEND": "redis"}))


def test_repos_parsing() -> None:
    c = load_from(map_lookup({"REPOS": " a/b , c/d ,, e/f "}))
    assert c.repos == ["a/b", "c/d", "e/f"]


def test_code_model_override() -> None:
    c = load_from(map_lookup({"OLLAMA_MODEL": "gemma4:12b", "OLLAMA_CODE_MODEL": "gemma4:26b"}))
    assert c.ollama_code_model == "gemma4:26b"
    assert c.ollama_model == "gemma4:12b"


def test_github_token_env_chain() -> None:
    # GH_TOKEN is honoured when GITHUB_TOKEN is unset, so a local gh-style env works.
    assert load_from(map_lookup({"GH_TOKEN": "gh_abc"})).github_token == "gh_abc"
    # GITHUB_TOKEN takes precedence over GH_TOKEN.
    c = load_from(map_lookup({"GITHUB_TOKEN": "primary", "GH_TOKEN": "fallback"}))
    assert c.github_token == "primary"


def test_git_transport_ssh() -> None:
    c = load_from(
        map_lookup({"GIT_TRANSPORT": "ssh", "GIT_SSH_KEY": "/home/dev/.ssh/id_ed25519"})
    )
    assert c.git_transport == "ssh"
    assert c.git_ssh_key == "/home/dev/.ssh/id_ed25519"


def test_invalid_git_transport() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"GIT_TRANSPORT": "scp"}))


def test_invalid_provider() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"LLM_PROVIDER": "openai"}))


def test_invalid_notify() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"NOTIFY_PROVIDER": "discord"}))


def test_invalid_duration() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"CI_TIMEOUT": "soon"}))


def test_max_iterations_floor() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"MAX_ITERATIONS": "0"}))


def test_max_iterations_unparseable() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"MAX_ITERATIONS": "lots"}))


def test_duration_compound() -> None:
    c = load_from(map_lookup({"CI_TIMEOUT": "1h30m"}))
    assert c.ci_timeout.total_seconds() == 90 * 60


def test_invalid_port_non_numeric() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"PORT": "abc"}))


def test_invalid_port_out_of_range() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"PORT": "70000"}))


# --- Execution transport (TASKS_BACKEND) -------------------------------------


def test_tasks_backend_defaults_inprocess() -> None:
    c = load_from(map_lookup({}))
    assert c.tasks_backend == TasksBackend.INPROCESS
    assert c.tasks_dispatch_deadline.total_seconds() == 30 * 60


def test_invalid_tasks_backend() -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup({"TASKS_BACKEND": "kafka"}))


def test_cloudtasks_requires_settings() -> None:
    # Missing everything but the backend selector.
    with pytest.raises(ValueError):
        load_from(map_lookup({"TASKS_BACKEND": "cloudtasks"}))
    # Drop one required key at a time from an otherwise-valid config.
    for key in (
        "TASKS_LOCATION",
        "TASKS_QUEUE",
        "DISPATCH_URL",
        "INTERNAL_TOKEN",
        "GITHUB_WEBHOOK_SECRET",
    ):
        env = full_cloudtasks_env()
        del env[key]
        with pytest.raises(ValueError):
            load_from(map_lookup(env))


def test_cloudtasks_rejects_insecure_dispatch_url() -> None:
    # DISPATCH_URL must be an absolute https URL ending in /internal/dispatch — the task
    # carries the Bearer token to it, so a plaintext / wrong-path target is rejected.
    for bad in (
        "http://svc.run.app/internal/dispatch",  # plaintext
        "/internal/dispatch",  # relative
        "not a url",  # garbage
        "https://svc.run.app/",  # right scheme, wrong path
    ):
        env = full_cloudtasks_env()
        env["DISPATCH_URL"] = bad
        with pytest.raises(ValueError):
            load_from(map_lookup(env))


def test_cloudtasks_rejects_out_of_range_deadline() -> None:
    # Cloud Tasks clamps an HTTP-target dispatch deadline to 15s..30m.
    for bad in ("10s", "45m"):
        env = full_cloudtasks_env()
        env["TASKS_DISPATCH_DEADLINE"] = bad
        with pytest.raises(ValueError):
            load_from(map_lookup(env))


def test_cloudtasks_full_config() -> None:
    c = load_from(map_lookup(full_cloudtasks_env()))
    assert c.tasks_backend == TasksBackend.CLOUDTASKS
    assert c.tasks_project == "proj"
    assert c.tasks_location == "us-central1"
    assert c.tasks_queue == "agent-q"
    assert c.dispatch_url == "https://svc.run.app/internal/dispatch"


def test_tasks_project_falls_back_to_google_cloud_project() -> None:
    # TASKS_PROJECT falls back to GOOGLE_CLOUD_PROJECT (the ambient Cloud Run var).
    env = full_cloudtasks_env()
    del env["TASKS_PROJECT"]
    env["GOOGLE_CLOUD_PROJECT"] = "ambient"
    c = load_from(map_lookup(env))
    assert c.tasks_project == "ambient"


# --- Observability (OTEL_*) --------------------------------------------------


def test_otel_defaults() -> None:
    # Off by default: the no-op exporter, the service name, and always-on sampling, with
    # message-content capture off.
    c = load_from(map_lookup({}))
    assert c.otel_traces_exporter == "none"
    assert c.otel_service_name == "automation-agent"
    assert c.otel_exporter_otlp_endpoint == ""
    assert c.otel_exporter_otlp_headers == ""
    assert c.otel_traces_sampler == "parentbased_always_on"
    assert c.otel_capture_message_content is False


def test_otel_console_and_gcp_need_no_endpoint() -> None:
    for exporter in ("console", "gcp"):
        c = load_from(map_lookup({"OTEL_TRACES_EXPORTER": exporter}))
        assert c.otel_traces_exporter == exporter


def test_otel_otlp_requires_endpoint() -> None:
    with pytest.raises(ValueError, match="OTEL_EXPORTER_OTLP_ENDPOINT"):
        load_from(map_lookup({"OTEL_TRACES_EXPORTER": "otlp"}))


def test_otel_otlp_full() -> None:
    c = load_from(
        map_lookup(
            {
                "OTEL_TRACES_EXPORTER": "otlp",
                "OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com",
                "OTEL_EXPORTER_OTLP_HEADERS": "api-key=secret",
                "OTEL_SERVICE_NAME": "my-agent",
            }
        )
    )
    assert c.otel_traces_exporter == "otlp"
    assert c.otel_exporter_otlp_endpoint == "https://otlp.example.com"
    assert c.otel_service_name == "my-agent"


def test_otel_unknown_exporter_rejected() -> None:
    with pytest.raises(ValueError, match="invalid OTEL_TRACES_EXPORTER"):
        load_from(map_lookup({"OTEL_TRACES_EXPORTER": "jaeger"}))


def test_otel_capture_message_content_parsed() -> None:
    c = load_from(map_lookup({"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "true"}))
    assert c.otel_capture_message_content is True


def test_otel_headers_masked_in_repr() -> None:
    # The OTLP headers can carry a vendor API key, so the repr must mask them like every
    # other secret (never dump a credential to a startup log).
    c = load_from(
        map_lookup(
            {
                "OTEL_TRACES_EXPORTER": "otlp",
                "OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com",
                "OTEL_EXPORTER_OTLP_HEADERS": "api-key=secret",
            }
        )
    )
    text = repr(c)
    assert "api-key=secret" not in text
    assert "otel_exporter_otlp_headers='***'" in text
