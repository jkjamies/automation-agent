"""The model-calling review stage: the category fan-out, the glue drive, diff formatting, and the
per-agent instruction composition.

Returns the scorecard and the gated findings for the publish stage; posts nothing itself.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, cast

from google.adk.agents import BaseAgent, ParallelAgent
from google.adk.models import BaseLlm

from automation_agent.agent import setup
from automation_agent.agent.reviewer import standards as standards_mod
from automation_agent.agent.reviewer.categories import Category, Tier, select_categories
from automation_agent.agent.reviewer.findings import (
    Finding,
    findings_json,
    parse_findings,
)
from automation_agent.agent.reviewer.glue import (
    dedupe,
    demote_to_nitpick,
    drop_low_confidence,
)
from automation_agent.agent.reviewer.scorecard import Scorecard, score_findings
from automation_agent.agent.reviewer.standards import Standards
from automation_agent.githubapi import PRFile

if TYPE_CHECKING:
    from automation_agent.agent.reviewer.reviewer import Engine

# The user inputs that start each drive. The real instruction (lens prompt + diff) lives in the
# agents' system instruction; these just kick generation.
REVIEW_TRIGGER = "Review the diff and report findings as the JSON array specified."
GLUE_TRIGGER = "Synthesize the holistic findings as the JSON array specified."


async def run_review(
    engine: Engine, files: list[PRFile], std: Standards | None
) -> tuple[Scorecard, list[Finding]]:
    """Run the model-calling stage for a reviewable PR: fan out the category lenses, run the
    holistic glue pass, then apply the deterministic verify gate (confidence drop + dedup) and
    score. Returns the scorecard and the gated findings (the caller publishes them)."""
    diff = format_diff(files)
    cats = select_categories(files)

    category = await run_category_review(engine, diff, cats, std)
    # Glue sees the category findings as "already reported" and skips re-flagging them, so it must
    # see only the findings that survive the same gates as the final output. Otherwise a finding
    # the verify/citation gate later drops is suppressed in glue and then dropped here, vanishing
    # from the review entirely.
    gated_for_glue = standards_mod.gate_citations(
        engine, drop_low_confidence(list(category), engine.min_confidence), std
    )
    glue = await run_glue(engine, diff, gated_for_glue, std)

    all_findings = category + glue
    all_findings = drop_low_confidence(all_findings, engine.min_confidence)  # phase-1 verify gate
    all_findings = standards_mod.gate_citations(engine, all_findings, std)  # citation gate
    all_findings = dedupe(all_findings)  # cross-lens dedup
    return score_findings(all_findings), all_findings


async def run_category_review(
    engine: Engine, diff: str, cats: list[Category], std: Standards | None
) -> list[Finding]:
    """Build one agent per applicable category, run them in parallel (ADK ParallelAgent —
    genuine concurrency on Vertex, GPU-serialized locally with no code change), and return every
    category's parsed findings. Empty findings is success. The "(other)" catch-all's findings are
    demoted to nitpick."""
    # Deferred import breaks the review <-> agents_setup module cycle.
    from automation_agent.agent.reviewer import agents_setup

    agents: list[BaseAgent] = [
        agents_setup.build_category_agent(engine, c, diff, std) for c in cats
    ]
    parallel = ParallelAgent(
        name="review_all",
        description="Per-category review in parallel",
        sub_agents=agents,
    )
    runner = setup.new_runner("reviewer-review", parallel)
    state = await setup.drive_collect_state(runner, "system", "review", REVIEW_TRIGGER)

    out: list[Finding] = []
    for c in cats:
        key = findings_key(c.name)
        raw = state.get(key)
        if key not in state:
            # A lens that ran but found nothing is normal (empty = success); a missing state key
            # means it produced no output at all. Log it, but don't fail the whole review on one
            # lens — best-effort by design.
            engine.log.warning("category produced no findings output category=%s", c.name)
        found = parse_findings(raw if isinstance(raw, str) else "")
        if c.other:
            found = demote_to_nitpick(found)
        out.extend(found)
    return out


async def run_glue(
    engine: Engine, diff: str, prior: list[Finding], std: Standards | None
) -> list[Finding]:
    """Run the holistic synthesis pass over the diff and the category findings, returning the
    additional architectural/testability/coverage findings it produced. Empty is success."""
    from automation_agent.agent.reviewer import agents_setup

    agent = agents_setup.build_glue_agent(engine, diff, prior, std)
    runner = setup.new_runner("reviewer-glue", agent)
    text = await setup.drive_text(runner, "system", "glue", GLUE_TRIGGER)
    return parse_findings(text)


def format_diff(files: list[PRFile]) -> str:
    """Render the filtered files as one prompt-ready diff: a header per file plus its patch in a
    fenced block. A file with no patch (binary/oversized) is noted so the model knows it changed
    without a hunk to review."""
    parts: list[str] = []
    for f in files:
        if f.status == "renamed" and f.previous_path != "":
            parts.append(f"### {f.path} (renamed from {f.previous_path})\n")
        else:
            parts.append(f"### {f.path} ({f.status})\n")
        if f.patch.strip() == "":
            parts.append("(no textual diff available)\n\n")
            continue
        # Patch content is untrusted (it can be a diff of a Markdown/RST file that itself contains
        # ``` runs), so pick a fence longer than the longest backtick run in the patch — otherwise
        # an embedded run would close the block early and corrupt the prompt structure.
        fence = "`" * (max_backtick_run(f.patch) + 1)
        if len(fence) < 3:
            fence = "```"
        parts.append(fence + "diff\n")
        parts.append(f.patch)
        if not f.patch.endswith("\n"):
            parts.append("\n")
        parts.append(fence + "\n\n")
    return "".join(parts)


def max_backtick_run(s: str) -> int:
    """Return the length of the longest run of consecutive backticks in ``s`` (0 if none), used
    to size a fence that the content cannot break out of."""
    longest = 0
    cur = 0
    for ch in s:
        if ch == "`":
            cur += 1
            if cur > longest:
                longest = cur
        else:
            cur = 0
    return longest


def findings_key(name: str) -> str:
    """The session-state key a category agent writes its findings JSON to."""
    return "findings:" + name


def model_for_tier(engine: Engine, tier: Tier) -> BaseLlm:
    """Return the LLM a category runs on (code tier → code model, else base model)."""
    return engine.code_llm if tier is Tier.CODE else engine.base_llm


def build_review_instruction(prompt_body: str, diff: str, std: Standards | None) -> str:
    """Compose a category agent's instruction: the lens prompt, the repo's standards rule menu
    (when any), and the filtered diff (baked in because they are per-event)."""
    parts = [prompt_body]
    write_standards_menu(parts, std)
    parts.append("\n\n## Diff under review\n\n")
    parts.append(diff)
    return "".join(parts)


def build_glue_instruction(
    prompt_body: str, diff: str, prior: list[Finding], std: Standards | None
) -> str:
    """Compose the glue agent's instruction: the glue prompt, the standards menu, the diff, and
    the findings the category agents already produced (so it reasons holistically without
    re-flagging them)."""
    parts = [prompt_body]
    write_standards_menu(parts, std)
    parts.append("\n\n## Diff under review\n\n")
    parts.append(diff)
    parts.append("\n\n## Findings already reported by other lenses\n\n")
    parts.append(findings_json(prior))
    return "".join(parts)


def write_standards_menu(parts: list[str], std: Standards | None) -> None:
    """Append the repo's compact rule menu and the citation instruction to an agent prompt when
    standards were discovered. The full text of any rule is available via get_rule."""
    if standards_mod.is_empty(std):
        return
    real = cast("Standards", std)
    parts.append("\n\n## Repo standards (cite rule_id for conformance findings)\n\n")
    parts.append(real.menu())
    parts.append(
        "\nWhen a finding is a violation of one of these rules, set its dimension to the "
        "rule's dimension and set \"rule_id\" to the rule's id. Call get_rule(id) to read a "
        "rule's full text before flagging. Never invent a rule id; a pattern/architecture "
        "finding with no matching rule is not a standards violation.\n"
    )
