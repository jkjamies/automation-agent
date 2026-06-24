"""Tests for the status-aware terminal summary (build_summary_text). Pure, no I/O."""

from __future__ import annotations

from automation_agent.agent.fixflow.summary import (
    SummaryInput,
    TerminalOutcome,
    build_summary_text,
)
from automation_agent.githubapi import ChangedFile, Comparison


def _changed(n_commits: int = 1, files: list[str] | None = None) -> Comparison:
    return Comparison(
        total_commits=n_commits,
        files=[ChangedFile(path=p) for p in (files or ["a.py"])],
    )


def test_success_framing() -> None:
    text = build_summary_text(
        SummaryInput(
            outcome=TerminalOutcome.SUCCESS,
            workflow="lint",
            full_repo="acme/api",
            pr_number=42,
            attempts=1,
            report="2 issues in a.py",
            changed=_changed(1, ["a.py"]),
        )
    )
    assert "acme/api: the lint fix passed CI after 1 attempt." in text
    assert "1 commit changed a.py." in text
    assert "Targeted: 2 issues in a.py" in text


def test_exhausted_framing_uses_last_output() -> None:
    text = build_summary_text(
        SummaryInput(
            outcome=TerminalOutcome.EXHAUSTED,
            workflow="coverage",
            full_repo="acme/api",
            pr_number=7,
            attempts=3,
            last_output="still 2 uncovered lines",
            changed=_changed(2, ["a.py", "b.py"]),
        )
    )
    assert "still fails CI after 3 attempts. Please review." in text
    assert "2 commits changed a.py, b.py." in text
    assert "Remaining: still 2 uncovered lines" in text


def test_timeout_framing_names_check_and_window() -> None:
    text = build_summary_text(
        SummaryInput(
            outcome=TerminalOutcome.TIMEOUT,
            workflow="lint",
            full_repo="acme/api",
            pr_number=9,
            attempts=2,
            report="original findings",
            timeout="1:30:00",
            check_name="agent-lint-verify",
        )
    )
    assert "saw no CI result after 1:30:00 waiting for agent-lint-verify (2 attempts)" in text
    assert "Please review." in text
    assert "Targeted: original findings" in text
    # No comparison was supplied (default), so the changed-summary section says so.
    assert "No changes were recorded on the PR." in text


def test_no_changes_recorded() -> None:
    text = build_summary_text(
        SummaryInput(
            outcome=TerminalOutcome.SUCCESS,
            workflow="lint",
            full_repo="acme/api",
            pr_number=1,
            attempts=1,
            changed=Comparison(),
        )
    )
    assert "No changes were recorded on the PR." in text


def test_files_list_truncates_beyond_eight() -> None:
    files = [f"f{i}.py" for i in range(10)]
    text = build_summary_text(
        SummaryInput(
            outcome=TerminalOutcome.SUCCESS,
            workflow="lint",
            full_repo="acme/api",
            pr_number=1,
            attempts=1,
            changed=_changed(1, files),
        )
    )
    assert "(+2 more)" in text
    assert "f8.py" not in text  # only the first 8 are listed


def test_findings_truncated_and_whitespace_collapsed() -> None:
    blob = "line one\n\n   line two   " + ("x" * 400)
    text = build_summary_text(
        SummaryInput(
            outcome=TerminalOutcome.SUCCESS,
            workflow="lint",
            full_repo="acme/api",
            pr_number=1,
            attempts=1,
            report=blob,
            changed=Comparison(),
        )
    )
    assert "\n\n" not in text.split("Targeted: ", 1)[1]  # whitespace collapsed
    assert text.endswith("…")  # over the rune cap -> truncated


def test_empty_findings_appends_nothing() -> None:
    text = build_summary_text(
        SummaryInput(
            outcome=TerminalOutcome.SUCCESS,
            workflow="lint",
            full_repo="acme/api",
            pr_number=1,
            attempts=1,
            report="",
            changed=_changed(1, ["a.py"]),
        )
    )
    assert "Targeted:" not in text
