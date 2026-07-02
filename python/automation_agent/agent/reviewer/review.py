"""The model-calling review stage: the category fan-out, the glue drive, diff formatting, and the
per-agent instruction composition.

Returns the scorecard and the gated findings for the publish stage; posts nothing itself.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from google.adk.agents import BaseAgent, ParallelAgent
from google.adk.models import BaseLlm

from automation_agent.agent import setup
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
from automation_agent.githubapi import PRFile

if TYPE_CHECKING:
    from automation_agent.agent.reviewer.reviewer import Engine

# The user inputs that start each drive. The real instruction (lens prompt + diff) lives in the
# agents' system instruction; these just kick generation.
REVIEW_TRIGGER = "Review the diff and report findings as the JSON array specified."
GLUE_TRIGGER = "Synthesize the holistic findings as the JSON array specified."


async def run_review(engine: Engine, files: list[PRFile]) -> tuple[Scorecard, list[Finding]]:
    """Run the model-calling stage for a reviewable PR: fan out the category lenses, run the
    holistic glue pass, then apply the deterministic verify gate (confidence drop + dedup) and
    score. Returns the scorecard and the gated findings (the caller publishes them)."""
    diff = format_diff(files)
    cats = select_categories(files)

    category = await run_category_review(engine, diff, cats)
    # Glue sees the category findings as "already reported" and skips re-flagging them, so it must
    # see only the findings that survive the same gate as the final output. Otherwise a finding
    # the verify gate later drops is suppressed in glue and then dropped here, vanishing from the
    # review entirely.
    gated_for_glue = drop_low_confidence(list(category), engine.min_confidence)
    glue = await run_glue(engine, diff, gated_for_glue)

    all_findings = category + glue
    all_findings = drop_low_confidence(all_findings, engine.min_confidence)  # phase-1 verify gate
    all_findings = dedupe(all_findings)  # cross-lens dedup
    return score_findings(all_findings), all_findings


async def run_category_review(engine: Engine, diff: str, cats: list[Category]) -> list[Finding]:
    """Build one agent per applicable category, run them in parallel (ADK ParallelAgent —
    genuine concurrency on Vertex, GPU-serialized locally with no code change), and return every
    category's parsed findings. Empty findings is success. The "(other)" catch-all's findings are
    demoted to nitpick."""
    # Deferred import breaks the review <-> agents_setup module cycle.
    from automation_agent.agent.reviewer import agents_setup

    agents: list[BaseAgent] = [agents_setup.build_category_agent(engine, c, diff) for c in cats]
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


async def run_glue(engine: Engine, diff: str, prior: list[Finding]) -> list[Finding]:
    """Run the holistic synthesis pass over the diff and the category findings, returning the
    additional architectural/testability/coverage findings it produced. Empty is success."""
    from automation_agent.agent.reviewer import agents_setup

    agent = agents_setup.build_glue_agent(engine, diff, prior)
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


def build_review_instruction(prompt_body: str, diff: str) -> str:
    """Compose a category agent's instruction: the lens prompt and the filtered diff (baked in
    because it is per-event)."""
    parts = [prompt_body]
    parts.append("\n\n## Diff under review\n\n")
    parts.append(diff)
    return "".join(parts)


def build_glue_instruction(prompt_body: str, diff: str, prior: list[Finding]) -> str:
    """Compose the glue agent's instruction: the glue prompt, the diff, and the findings the
    category agents already produced (so it reasons holistically without re-flagging them)."""
    parts = [prompt_body]
    parts.append("\n\n## Diff under review\n\n")
    parts.append(diff)
    parts.append("\n\n## Findings already reported by other lenses\n\n")
    parts.append(findings_json(prior))
    return "".join(parts)
