"""The build-agent split: pure ADK wiring (category + glue + distiller LLM agents, the prompt
loader, the JSON generate-content config). Logic lives in the sibling modules.

The diff / standards are baked into each agent's system instruction because they are per-event;
the category and glue agents get the lazy ``get_rule`` tool when standards are present.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, cast

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
from automation_agent.agent.reviewer.standards import (
    Standards,
    build_distiller_instruction,
    standards_tools,
)

if TYPE_CHECKING:
    from automation_agent.agent.reviewer.reviewer import Engine

_prompts = setup.Prompts("automation_agent.agent.reviewer")


def build_category_agent(engine: Engine, c: Category, diff: str, std: Standards | None) -> LlmAgent:
    """Build one category review agent: an LLM agent on the category's tier whose instruction is
    the category prompt + the repo's standards rule menu (when any) + the filtered diff, writing
    its findings JSON to the category's state key. When standards are present it also gets the
    lazy get_rule tool."""
    body = _prompts.get(c.prompt_name)
    tools = standards_tools(std)
    return LlmAgent(
        name="review_" + c.name,
        description=c.title + " review",
        model=model_for_tier(engine, c.tier),
        instruction=build_review_instruction(body, diff, std),
        tools=cast("list[Any]", tools),
        output_key=findings_key(c.name),
        generate_content_config=setup.json_config(),
    )


def build_glue_agent(
    engine: Engine, diff: str, prior: list[Finding], std: Standards | None
) -> LlmAgent:
    """Build the glue/synthesis agent: a code-tier LLM agent that sees the diff, the category
    findings so far, and the repo's standards rule menu, emitting additional
    architectural-alignment / testability / test-coverage findings (cross-lens dedup is done
    deterministically in code, not here)."""
    body = _prompts.get("glue")
    tools = standards_tools(std)
    return LlmAgent(
        name="review_glue",
        description="Holistic synthesis review",
        model=engine.code_llm,
        instruction=build_glue_instruction(body, diff, prior, std),
        tools=cast("list[Any]", tools),
        generate_content_config=setup.json_config(),
    )


def build_distiller_agent(engine: Engine, docs: dict[str, str], sources: list[str]) -> LlmAgent:
    """Build the standards distiller: a base-tier LLM agent (distillation is
    summarization/extraction, the base tier) fed the reviewed repo's standards docs, prompted to
    emit a uniform tagged rule list. It normalizes heterogeneous formats into one list."""
    body = _prompts.get("distill")
    return LlmAgent(
        name="standards_distiller",
        description="Distill the repo's standards docs into a tagged rule list",
        model=engine.base_llm,
        instruction=build_distiller_instruction(body, docs, sources),
        generate_content_config=setup.json_config(),
    )
