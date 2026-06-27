"""Triage: normalize an arbitrary linter report into per-file work (LLM-backed).

Keeps the lint-fixer agnostic to the reporting format.
"""

from __future__ import annotations

import json

from google.adk.models import BaseLlm

from automation_agent.agent import setup
from automation_agent.agent.fixflow import FileWork, NoWorkError, extract_json_array
from automation_agent.agent.lintfixer.loader import prompts


async def triage(llm: BaseLlm, report: str) -> list[FileWork]:
    """Use the LLM to normalize a linter report into per-file work."""
    out = await setup.generate_text(llm, prompts.must_get("triage"), report)
    work = _parse_triage(out)
    if not work:
        raise NoWorkError("triage: no actionable files found in report")
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
            problems = f.get("problems") or []
            work.append(FileWork(path=path, items=list(problems)))
    return work
