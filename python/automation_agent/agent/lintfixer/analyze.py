"""Analyze: rewrite each affected source file to fix its lint problems.

Port of ``lintfixer/analyze.go``. One parallel agent per file, reading the current
source from the checkout. Feedback (on retry) is the previous attempt's CI failure.
"""

from __future__ import annotations

from automation_agent.agent import setup
from automation_agent.agent.fixflow import (
    AnalyzeInput,
    FileEdit,
    FileWork,
    parallel_analyze,
    read_file,
    strip_fences,
)
from automation_agent.agent.lintfixer.loader import prompts


async def analyze(in_: AnalyzeInput) -> list[FileEdit]:
    """Rewrite each affected source file to fix its lint problems, in parallel."""

    async def edit(w: FileWork) -> FileEdit:
        try:
            src = read_file(in_.repo_dir, w.path)
        except (OSError, ValueError):
            # Unreadable file (incl. a path that escapes the repo root, which
            # read_file rejects with ValueError) -> skip, matching Go's
            # "any read error -> skip" behavior.
            return FileEdit(path="", content="")
        out = await setup.generate_text(
            in_.coder(), prompts.must_get("analyze"), _build_file_prompt(w, src, in_.feedback)
        )
        return FileEdit(path=w.path, content=strip_fences(out))

    return await parallel_analyze(in_.work, edit)


def _build_file_prompt(w: FileWork, content: str, ci_feedback: str) -> str:
    lines = [f"File: {w.path}\n", "Lint problems to fix:"]
    for p in w.items:
        lines.append(f"- {p}")
    body = "\n".join(lines)
    if ci_feedback != "":
        body += "\n\nThe previous attempt failed CI with:\n" + ci_feedback
    body += (
        "\n\nCurrent file content:\n```\n"
        + content
        + "\n```\n\nOutput ONLY the complete corrected content of this file — "
        "no explanation, no markdown fences."
    )
    return body
