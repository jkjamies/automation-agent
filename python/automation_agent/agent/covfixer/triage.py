"""Triage: normalize an arbitrary coverage report into source files with meaningful
uncovered logic (LLM-backed).
"""

from __future__ import annotations

import json

from google.adk.models import BaseLlm

from automation_agent.agent import setup
from automation_agent.agent.covfixer.loader import prompts
from automation_agent.agent.fixflow import FileWork, extract_json_array


async def triage(llm: BaseLlm, report: str) -> list[FileWork]:
    """Use the LLM to normalize a coverage report into the source files with meaningful
    uncovered logic."""
    out = await setup.generate_text(llm, prompts.must_get("triage"), report)
    work = _parse_triage(out)
    if not work:
        raise ValueError("triage: no meaningful uncovered files found in report")
    return work


def _parse_triage(out: str) -> list[FileWork]:
    js = extract_json_array(out)
    if js == "":
        raise ValueError("triage: no JSON array in model output")
    try:
        files = json.loads(js)
    except (ValueError, TypeError) as exc:
        raise ValueError(f"triage: decode triage JSON: {exc}") from exc
    work: list[FileWork] = []
    for f in files:
        path = (f.get("path") or "") if isinstance(f, dict) else ""
        if isinstance(path, str) and path.strip() != "":
            uncovered = f.get("uncovered") or []
            work.append(FileWork(path=path, items=list(uncovered)))
    return work
