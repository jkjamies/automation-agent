"""Tests for config loading."""

from __future__ import annotations

import pytest

from automation_agent.config import (
    NotifyProvider,
    Provider,
    SessionBackend,
    load_from,
)


def map_lookup(m: dict[str, str]):
    return m.get


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
