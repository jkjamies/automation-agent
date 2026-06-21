"""A simple chat agent over the configured model, for local development only.

Launched via ``make playground`` (``adk web cmd/playground``). This is the Python twin
of ``cmd/playground/main.go`` — development only, never part of a deployed artifact.
Swap in the summary / lintfixer / covfixer agents here to drive the real workflows
interactively.
"""

from __future__ import annotations

from dotenv import load_dotenv
from google.adk.agents import LlmAgent

from automation_agent.agent import setup
from automation_agent.config import load

load_dotenv()  # so you don't need to `source .env` first
_cfg = load()

root_agent = LlmAgent(
    name="automation_agent_playground",
    description="Local playground for poking the configured model.",
    model=setup.build_llm(_cfg),
    instruction=(
        "You are the automation-agent local playground, backed by the model "
        f"'{_cfg.ollama_model}'. Help the developer test prompts. Be concise."
    ),
)
