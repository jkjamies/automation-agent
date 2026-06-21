"""The LLM provider switch.

This is the **only** module permitted to import provider SDKs (LiteLlm for the
local Ollama/Gemma path, Gemini for the cloud path) — enforced by the arch tests.
Agents depend only on the returned ``BaseLlm``, so switching providers is a config
change, not a code change. See ``docs/architecture.md`` §4.

ADK Python ships Ollama support via LiteLLM, so the local path uses
``LiteLlm(model="ollama_chat/<model>")``.
"""

from __future__ import annotations

from google.adk.models import BaseLlm, Gemini
from google.adk.models.lite_llm import LiteLlm

from automation_agent.config import Config, Provider

# Deterministic generation settings for the Ollama path: temperature 0 and a large
# context window to avoid silently truncating big source files.
_OLLAMA_TEMPERATURE = 0.0
_OLLAMA_NUM_CTX = 32768


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
        )
    if cfg.llm_provider == Provider.GEMINI:
        if not gemini_model:
            raise ValueError("gemini model not configured")
        return Gemini(model=gemini_model)
    raise ValueError(f"unknown LLM provider {cfg.llm_provider!r}")
