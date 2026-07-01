"""A simple chat agent over the configured model, for local development only.

Launched via ``make playground`` (``adk web cmd/playground``). Development only, never
part of a deployed artifact. Swap in the summary / lintfixer / covfixer agents here to
drive the real workflows interactively.
"""

from __future__ import annotations

import atexit

from dotenv import load_dotenv
from google.adk.agents import LlmAgent

from automation_agent.agent import setup
from automation_agent.config import OTEL_EXPORTER_CONSOLE, load
from automation_agent.obs import Config as ObsConfig
from automation_agent.obs import init as init_tracing

load_dotenv()  # so you don't need to `source .env` first
_cfg = load()

# Default the playground to the console exporter so a developer sees the span tree on stdout
# with no backend to stand up — but respect an explicit OTEL_TRACES_EXPORTER (config records
# whether one was provided, so this module never reads the environment itself). Registered at
# import because `adk web` loads this module and drives root_agent (there is no run() to defer
# to); atexit force-flushes the buffered spans when the dev process ends.
_exporter = _cfg.otel_traces_exporter if _cfg.otel_traces_exporter_set else OTEL_EXPORTER_CONSOLE
_shutdown_tracing = init_tracing(
    ObsConfig(
        exporter=_exporter,
        service_name=_cfg.otel_service_name,
        otlp_endpoint=_cfg.otel_exporter_otlp_endpoint,
        otlp_headers=_cfg.otel_exporter_otlp_headers,
        sampler=_cfg.otel_traces_sampler,
    )
)
atexit.register(_shutdown_tracing)

root_agent = LlmAgent(
    name="automation_agent_playground",
    description="Local playground for poking the configured model.",
    model=setup.build_llm(_cfg),
    instruction=(
        "You are the automation-agent local playground, backed by the configured "
        "model. Help the developer test prompts. Be concise."
    ),
)
