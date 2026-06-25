"""The LLM provider switch.

This is the **only** module permitted to import provider SDKs (LiteLlm for the
local Ollama/Gemma path, Gemini for the cloud path) — enforced by the arch tests.
Agents depend only on the returned ``BaseLlm``, so switching providers is a config
change, not a code change. See ``.agents/standards/architecture-design.md`` §4.

ADK Python ships Ollama support via LiteLLM, so the local path uses
``LiteLlm(model="ollama_chat/<model>")``.
"""

from __future__ import annotations

import httpx
from google.adk.models import BaseLlm, Gemini
from google.adk.models.lite_llm import LiteLlm

from automation_agent.config import Config, Provider

# Deterministic generation settings for the Ollama path: temperature 0 and a large
# context window to avoid silently truncating big source files.
_OLLAMA_TEMPERATURE = 0.0
_OLLAMA_NUM_CTX = 32768

# HTTP timeouts for the Ollama path. The runner streams over SSE (see
# ``runner.STREAMING_RUN_CONFIG``), so a long generation streams token-by-token over a
# long-lived body. ``read`` is therefore the max gap *between* streamed chunks — a
# cold-start / prefill cushion for the first chunk (a 26b model can be slow to load), not a
# cap on total generation time. There is deliberately **no** overall/total timeout: httpx
# has no wall-clock cap, so a long decode can take as long as the hardware needs. Without
# this, litellm's default (~600s, applied to ``read``) would truncate a slow generation.
# Note: litellm's ollama_chat path does not support an ``httpx.Timeout`` object
# (``supports_httpx_timeout`` excludes it), so it coerces this to the ``read`` value; the
# named fields document intent and are honored as-is should that provider list ever include
# ollama.
_OLLAMA_CONNECT_TIMEOUT = 10.0
_OLLAMA_READ_TIMEOUT = 300.0  # 5 min first-chunk cushion (cold model load + prefill)
_OLLAMA_TIMEOUT = httpx.Timeout(
    connect=_OLLAMA_CONNECT_TIMEOUT,
    read=_OLLAMA_READ_TIMEOUT,
    write=_OLLAMA_CONNECT_TIMEOUT,
    pool=_OLLAMA_CONNECT_TIMEOUT,
)


def build_llm(cfg: Config) -> BaseLlm:
    """Return the default model (triage, explore, summary) for the configured provider."""
    return _build_llm(cfg, cfg.ollama_model, cfg.gemini_model)


def build_code_llm(cfg: Config) -> BaseLlm:
    """Return the model for code-change steps (lint rewrite, coverage test generation),
    typically a larger model; falls back to the default when none is configured."""
    return _build_llm(cfg, cfg.ollama_code_model, cfg.gemini_code_model)


def _build_llm(cfg: Config, ollama_model: str, gemini_model: str) -> BaseLlm:
    if cfg.llm_provider == Provider.OLLAMA:
        return LiteLlm(
            model=f"ollama_chat/{ollama_model}",
            api_base=cfg.ollama_host,
            temperature=_OLLAMA_TEMPERATURE,
            num_ctx=_OLLAMA_NUM_CTX,
            timeout=_OLLAMA_TIMEOUT,
        )
    if cfg.llm_provider == Provider.GEMINI:
        if not gemini_model:
            raise ValueError("gemini model not configured")
        return Gemini(model=gemini_model)
    raise ValueError(f"unknown LLM provider {cfg.llm_provider!r}")
