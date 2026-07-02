"""The build-agent split: pure ADK wiring (category + glue LLM agents, the prompt loader, the
JSON generate-content config). Logic lives in the sibling modules.

The diff is baked into each agent's system instruction because it is per-event.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from google.adk.agents import LlmAgent

from automation_agent.agent import setup
from automation_agent.agent.reviewer.categories import Category
from automation_agent.agent.reviewer.findings import Finding
from automation_agent.agent.reviewer.review import (
    build_glue_instruction,
    build_review_instruction,
    findings_key,
    model_for_tier,
)

if TYPE_CHECKING:
    from automation_agent.agent.reviewer.reviewer import Engine

_prompts = setup.Prompts("automation_agent.agent.reviewer")


def build_category_agent(engine: Engine, c: Category, diff: str) -> LlmAgent:
    """Build one category review agent: an LLM agent on the category's tier whose instruction is
    the category prompt + the filtered diff, writing its findings JSON to the category's state
    key."""
    body = _prompts.get(c.prompt_name)
    return LlmAgent(
        name="review_" + c.name,
        description=c.title + " review",
        model=model_for_tier(engine, c.tier),
        instruction=build_review_instruction(body, diff),
        output_key=findings_key(c.name),
        generate_content_config=setup.json_config(),
    )


def build_glue_agent(engine: Engine, diff: str, prior: list[Finding]) -> LlmAgent:
    """Build the glue/synthesis agent: a code-tier LLM agent that sees the diff and the category
    findings so far, emitting additional architectural-alignment / testability / test-coverage
    findings (cross-lens dedup is done deterministically in code, not here)."""
    body = _prompts.get("glue")
    return LlmAgent(
        name="review_glue",
        description="Holistic synthesis review",
        model=engine.code_llm,
        instruction=build_glue_instruction(body, diff, prior),
        generate_content_config=setup.json_config(),
    )
