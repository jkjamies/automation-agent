"""Analyze: a two-phase test-generation step (explore a plan, then execute it).

:func:`explore` runs a tool-using agent that navigates
the checkout itself (read_file / list_dir) to learn the repo's real test conventions and
returns a per-file plan; :func:`execute` generates a test per file from that plan + the
source, one parallel agent per file.
"""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass

from automation_agent.agent import setup
from automation_agent.agent.covfixer.loader import prompts
from automation_agent.agent.fixflow import (
    AnalyzeInput,
    FileEdit,
    FileWork,
    parallel_analyze,
    read_file,
    strip_fences,
)
from automation_agent.agent.fixflow import (
    explore as fixflow_explore,
)

log = logging.getLogger(__name__)


@dataclass
class PlanEntry:
    """The explorer's decision for one source file, grounded in the repo's actual
    existing tests (never derived from a fixed rule)."""

    source: str
    test_path: str
    framework: str
    notes: str


async def analyze(in_: AnalyzeInput) -> list[FileEdit]:
    """Plan test placement by examining the repo's real conventions, then generate a
    test per file from that plan."""
    plan = await _explore(in_)
    return await _execute(in_, plan)


async def _explore(in_: AnalyzeInput) -> dict[str, PlanEntry]:
    out = await fixflow_explore(
        in_.llm, in_.repo_dir, prompts.must_get("explore"), _build_explore_input(in_.work)
    )
    plan = _parse_plan(out)
    if not plan:
        raise ValueError("explore: produced no test placements")
    return plan


async def _execute(
    in_: AnalyzeInput, plan: dict[str, PlanEntry]
) -> list[FileEdit]:
    async def edit(w: FileWork) -> FileEdit:
        p = plan.get(w.path)
        if p is None or p.test_path.strip() == "":
            log.warning("coverage analyze: explorer placed no test for %s -> skip", w.path)
            return FileEdit(path="", content="")  # explorer couldn't place it -> skip
        try:
            src = read_file(in_.repo_dir, w.path)
        except (OSError, ValueError) as exc:
            # Unreadable file (incl. a path escaping the repo root) -> skip:
            # any read error means skip. Log so a skip is distinguishable from
            # "nothing to do".
            log.warning("coverage analyze: skipping unreadable file %s: %s", w.path, exc)
            return FileEdit(path="", content="")
        out = await setup.generate_text(
            in_.coder(), prompts.must_get("analyze"), _build_execute_input(w, src, p, in_.feedback)
        )
        return FileEdit(path=p.test_path, content=strip_fences(out))

    return await parallel_analyze(in_.work, edit)


def _parse_plan(out: str) -> dict[str, PlanEntry]:
    from automation_agent.agent.fixflow import extract_json_array

    js = extract_json_array(out)
    if js == "":
        raise ValueError("explore: no JSON array in explorer output")
    try:
        entries = json.loads(js)
    except (ValueError, TypeError) as exc:
        raise ValueError(f"explore: decode plan JSON: {exc}") from exc
    m: dict[str, PlanEntry] = {}
    for e in entries:
        if not isinstance(e, dict):
            continue
        source = e.get("source") or ""
        if isinstance(source, str) and source.strip() != "":
            m[source] = PlanEntry(
                source=source,
                test_path=e.get("test_path") or "",
                framework=e.get("framework") or "",
                notes=e.get("notes") or "",
            )
    return m


def _build_explore_input(work: list[FileWork]) -> str:
    lines = ["Source files that need tests:"]
    for w in work:
        lines.append(f"- {w.path}")
    return "\n".join(lines) + "\n"


def _build_execute_input(
    w: FileWork, src: str, p: PlanEntry, ci_feedback: str
) -> str:
    body = f"Write the test file at: {p.test_path}\nFramework / convention: {p.framework}\n"
    if p.notes.strip() != "":
        body += f"Notes: {p.notes}\n"
    body += "\nUncovered logic to cover:\n"
    for u in w.items:
        body += f"- {u}\n"
    if ci_feedback != "":
        body += "\nThe previous attempt failed CI with:\n" + ci_feedback + "\n"
    body += f"\nSource file ({w.path}):\n```\n{src}\n```\n"
    return body
