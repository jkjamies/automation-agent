"""Tests for config loading."""

from __future__ import annotations

import pytest

from automation_agent.config import NotifyProvider, Provider, load_from


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
    assert c.agent_check_name == "agent-lint-verify"


def test_repos_parsing() -> None:
    c = load_from(map_lookup({"REPOS": " a/b , c/d ,, e/f "}))
    assert c.repos == ["a/b", "c/d", "e/f"]


def test_code_model_override() -> None:
    c = load_from(map_lookup({"OLLAMA_MODEL": "gemma4:12b", "OLLAMA_CODE_MODEL": "gemma4:26b"}))
    assert c.ollama_code_model == "gemma4:26b"
    assert c.ollama_model == "gemma4:12b"


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
