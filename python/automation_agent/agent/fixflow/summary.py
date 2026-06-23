"""The status-aware terminal summary for a finished fix run.

:func:`build_summary_text` frames how a run ended (success / exhausted attempts / timeout)
into a human notification body, enriched with the original targeted findings and what
actually changed on the PR (a base...head comparison). Pure (no I/O) so it is unit-testable;
the Driver gathers the inputs and sends the result.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum

from automation_agent.githubapi import ChangedFile, Comparison


class TerminalOutcome(Enum):
    """How a fix run ended; selects the summary framing."""

    SUCCESS = "success"
    EXHAUSTED = "exhausted"
    TIMEOUT = "timeout"


@dataclass
class SummaryInput:
    """Everything a terminal summary needs. The per-attempt work product lives only on the
    PR (commits + diff), never the session, so ``changed`` (a base...head comparison) is how
    the human learns what the agent actually did."""

    outcome: TerminalOutcome
    workflow: str  # spec.name (lint | coverage)
    full_repo: str
    pr_number: int
    attempts: int
    report: str = ""  # original targeted findings (run params report)
    last_output: str = ""  # last CI check output (exhausted) — the remaining findings
    timeout: str = ""  # CI_TIMEOUT (timeout outcome)
    check_name: str = ""  # the awaited check (timeout outcome)
    changed: Comparison = field(default_factory=Comparison)


# How much of a (potentially large) findings blob a summary inlines.
_MAX_FINDINGS_RUNES = 280
# How many changed-file names a summary lists before truncating.
_MAX_FILES = 8


def build_summary_text(in_: SummaryInput) -> str:
    """Frame a terminal outcome into a human notification body, enriched with the original
    findings and what changed on the PR."""
    changed = _changed_summary(in_.changed)
    if in_.outcome == TerminalOutcome.SUCCESS:
        text = (
            f"{in_.full_repo}: the {in_.workflow} fix passed CI after "
            f"{_attempts_phrase(in_.attempts)}. {changed}"
        )
        return _append_findings(text, "Targeted", in_.report)
    if in_.outcome == TerminalOutcome.EXHAUSTED:
        text = (
            f"{in_.full_repo}: the {in_.workflow} fix still fails CI after "
            f"{_attempts_phrase(in_.attempts)}. Please review. {changed}"
        )
        return _append_findings(text, "Remaining", in_.last_output)
    if in_.outcome == TerminalOutcome.TIMEOUT:
        text = (
            f"{in_.full_repo}: the {in_.workflow} fix saw no CI result after "
            f"{in_.timeout} waiting for {in_.check_name} "
            f"({_attempts_phrase(in_.attempts)}). Please review. {changed}"
        )
        return _append_findings(text, "Targeted", in_.report)
    return f"{in_.full_repo}: the {in_.workflow} fix reached an unknown terminal state."


def _attempts_phrase(n: int) -> str:
    return "1 attempt" if n == 1 else f"{n} attempts"


def _changed_summary(c: Comparison) -> str:
    """Describe the commits + files of a comparison, truncating a long file list."""
    if c.total_commits == 0 and not c.files:
        return "No changes were recorded on the PR."
    commits = "1 commit" if c.total_commits == 1 else f"{c.total_commits} commits"
    return f"{commits} changed {_files_phrase(c.files)}."


def _files_phrase(files: list[ChangedFile]) -> str:
    if not files:
        return "no files"
    names = [f.path for f in files]
    suffix = ""
    if len(names) > _MAX_FILES:
        suffix = f" (+{len(names) - _MAX_FILES} more)"
        names = names[:_MAX_FILES]
    return ", ".join(names) + suffix


def _append_findings(text: str, label: str, blob: str) -> str:
    """Add a single-line, length-bounded findings snippet to text, or return text unchanged
    when the blob is empty."""
    snippet = " ".join(blob.split())  # collapse newlines/whitespace
    if snippet == "":
        return text
    if len(snippet) > _MAX_FINDINGS_RUNES:
        snippet = snippet[:_MAX_FINDINGS_RUNES] + "…"
    return f"{text}\n{label}: {snippet}"
