"""Tests for the rest of the setup layer: prompt, events, generate, runner, llm."""

from __future__ import annotations

import httpx
import pytest
from google.adk.agents import LlmAgent
from google.adk.agents.run_config import StreamingMode
from google.adk.models.lite_llm import LiteLlm

from automation_agent.agent.setup.events import (
    assistant_text,
    content_text,
    last_text,
    state_string,
    text_event,
    user_text,
)
from automation_agent.agent.setup.generate import generate_text
from automation_agent.agent.setup.llm import build_code_llm, build_llm
from automation_agent.agent.setup.prompt import Prompts
from automation_agent.agent.setup.runner import (
    STREAMING_RUN_CONFIG,
    drive_collect_state,
    drive_text,
    new_runner,
)
from automation_agent.config import Config, Provider

# ---- prompt -----------------------------------------------------------------


def test_prompts_get_and_must_get() -> None:
    p = Prompts("automation_agent.agent.summary")
    body = p.get("summarize")
    assert body and body == body.strip()
    assert p.must_get("summarize") == body


def test_prompts_missing_raises() -> None:
    p = Prompts("automation_agent.agent.summary")
    with pytest.raises(FileNotFoundError):
        p.get("does-not-exist")


# ---- events -----------------------------------------------------------------


def test_content_helpers() -> None:
    assert content_text(user_text("hi")) == "hi"
    assert content_text(assistant_text("yo")) == "yo"
    assert content_text(None) == ""
    assert last_text([user_text("a"), assistant_text("b")]) == "b"
    assert last_text([]) == ""


def test_text_event_with_state() -> None:
    ev = text_event("fetcher", "digest text", {"commits:a/b": "x"})
    assert ev.author == "fetcher"
    assert content_text(ev.content) == "digest text"
    assert ev.actions.state_delta == {"commits:a/b": "x"}


def test_text_event_without_state() -> None:
    ev = text_event("notifier", "hello")
    assert not ev.actions.state_delta


def test_state_string() -> None:
    assert state_string({"k": "v"}, "k") == "v"
    assert state_string({"k": 3}, "k") == ""
    assert state_string({}, "missing") == ""


# ---- generate ---------------------------------------------------------------


async def test_generate_text(fake_llm) -> None:
    llm = fake_llm("the answer")
    out = await generate_text(llm, "you are a bot", "do the thing")
    assert out == "the answer"
    req = llm.requests[0]
    assert req.config.system_instruction == "you are a bot"
    assert content_text(req.contents[0]) == "do the thing"


# ---- runner -----------------------------------------------------------------


async def test_drive_text(fake_llm) -> None:
    agent = LlmAgent(name="echo", model=fake_llm("final answer"), instruction="x")
    runner = new_runner("t", agent)
    out = await drive_text(runner, "u", "s1", "hello")
    assert "final answer" in out


def test_streaming_run_config_is_sse() -> None:
    # Every runner call site shares this config so a long Ollama generation streams over a
    # long-lived body instead of being capped by a single buffered-response timeout.
    assert STREAMING_RUN_CONFIG.streaming_mode == StreamingMode.SSE


async def test_drive_collect_state() -> None:
    from google.adk.agents import BaseAgent
    from google.adk.events import Event, EventActions

    class Writer(BaseAgent):
        async def _run_async_impl(self, ctx):  # type: ignore[override]
            yield Event(author=self.name, actions=EventActions(state_delta={"a": 1}))
            yield Event(author=self.name, actions=EventActions(state_delta={"b": 2}))

    runner = new_runner("t", Writer(name="w"))
    state = await drive_collect_state(runner, "u", "s1", "go")
    assert state == {"a": 1, "b": 2}


# ---- llm switch -------------------------------------------------------------


def test_build_llm_ollama() -> None:
    cfg = Config(llm_provider=Provider.OLLAMA, ollama_model="gemma4:12b", ollama_code_model="gemma4:26b")
    base = build_llm(cfg)
    code = build_code_llm(cfg)
    assert isinstance(base, LiteLlm)
    assert base.model == "ollama_chat/gemma4:12b"
    assert code.model == "ollama_chat/gemma4:26b"


def test_build_llm_ollama_timeout() -> None:
    # The Ollama path sets a generous first-chunk (read) cushion with no overall/total cap,
    # so a long streaming generation is never truncated. read is the max gap between chunks.
    cfg = Config(llm_provider=Provider.OLLAMA, ollama_model="gemma4:12b")
    timeout = build_llm(cfg)._additional_args["timeout"]
    assert isinstance(timeout, httpx.Timeout)
    assert timeout.read == 300.0
    assert timeout.connect == 10.0


def test_build_llm_gemini_requires_model() -> None:
    cfg = Config(llm_provider=Provider.GEMINI, gemini_model="")
    with pytest.raises(ValueError):
        build_llm(cfg)


def test_build_llm_gemini() -> None:
    cfg = Config(llm_provider=Provider.GEMINI, gemini_model="gemini-flash-latest")
    base = build_llm(cfg)
    assert base.model == "gemini-flash-latest"
