"""The CodeRabbit-style publish stage: assembly + REST writes (advisory review, marker summary
comment, advisory agent-review check), reconciled against the PR's existing fingerprinted
comments. Nothing here gates a merge.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from automation_agent.agent.reviewer.findings import Dimension, Finding, Severity
from automation_agent.agent.reviewer.hunks import DiffIndex
from automation_agent.agent.reviewer.reconcile import fp_marker, reconcile
from automation_agent.agent.reviewer.review import max_backtick_run
from automation_agent.agent.reviewer.scorecard import Level, Scorecard
from automation_agent.githubapi import (
    CheckRunInput,
    PRFile,
    ReviewComment,
    ReviewInput,
)

if TYPE_CHECKING:
    from automation_agent.agent.reviewer.reviewer import Engine

# The advisory check the reviewer publishes (agent-published, human-consumed). Globally unique
# and identical across ports (external contract).
CHECK_NAME = "agent-review"


@dataclass
class PublishMeta:
    """Carries the per-PR identifiers and context the published artifacts need."""

    owner: str
    repo: str
    number: int
    head_sha: str
    files: list[PRFile] = field(default_factory=list)  # for the in-diff index
    tiers: str = ""  # model tiers used, for the Review details section
    standards: list[str] = field(default_factory=list)  # applied source paths (empty = generic)


def summary_marker(owner: str, repo: str, number: int) -> str:
    """The hidden HTML comment that identifies the reviewer's single summary comment so a
    re-review updates it rather than posting a new one."""
    return f"<!-- automation-agent:review:{owner}/{repo}#{number} -->"


def publish(engine: Engine, card: Scorecard, findings: list[Finding], meta: PublishMeta) -> None:
    """Post the review for a scored PR: inline comments for in-diff actionable findings, a
    marker-updated summary comment with the scorecard, and the advisory agent-review check.
    Out-of-diff actionable findings and nitpicks go into the summary (never dropped)."""
    # At-least-once safety: reconciliation makes the inline comments idempotent, but the check run
    # and summary are create/upsert-only, so a redelivered task for a SHA already published would
    # duplicate the check. If the agent-review check already exists for this head SHA, skip — a
    # genuine re-push carries a new SHA and still reconciles below.
    if already_published(engine, meta):
        engine.log.info(
            "review already published for head SHA; skipping re-post repo=%s/%s sha=%s",
            meta.owner,
            meta.repo,
            meta.head_sha,
        )
        return
    idx = DiffIndex(meta.files)
    inline, out_of_diff, nitpicks = classify(findings, idx)
    actionable = len(inline) + len(out_of_diff)

    # Reconcile against the comments already on the PR (GitHub-as-store): keep inline findings
    # that still apply (don't re-post — idempotent), post only new ones, and minimize the comments
    # whose finding is gone.
    existing = engine.gh.list_review_comments(meta.owner, meta.repo, meta.number)
    rec = reconcile(inline, existing)

    # Post only the new inline findings; an empty review is noise.
    if rec.to_post:
        comments = [
            ReviewComment(path=f.file, line=f.line, side="RIGHT", body=inline_comment_body(f))
            for f in rec.to_post
        ]
        body = (
            f"{card.overall.glyph()} Agent review — see the summary comment for the full scorecard."
        )
        engine.gh.create_review(
            meta.owner, meta.repo, meta.number, ReviewInput(body=body, comments=comments)
        )

    # Minimize the comments whose finding no longer applies — best-effort. New inline comments are
    # already posted but the summary and check are not; aborting here on a single minimize failure
    # would leave the PR without its summary/check. So log and continue per node.
    for node_id in rec.to_minimize:
        try:
            engine.gh.minimize_comment(node_id)
        except Exception as exc:  # noqa: BLE001
            engine.log.warning(
                "reviewer: minimize outdated comment failed; continuing repo=%s/%s node=%s err=%s",
                meta.owner,
                meta.repo,
                node_id,
                exc,
            )

    marker = summary_marker(meta.owner, meta.repo, meta.number)
    engine.gh.upsert_marker_comment(
        meta.owner,
        meta.repo,
        meta.number,
        marker,
        summary_comment(marker, card, actionable, nitpicks, out_of_diff, meta),
    )

    engine.gh.create_check_run(
        meta.owner,
        meta.repo,
        CheckRunInput(
            name=CHECK_NAME,
            head_sha=meta.head_sha,
            conclusion=check_conclusion(card.overall),
            title=f"{card.overall.glyph()} Agent review — {card.overall.word()}",
            summary=f"Overall: {card.overall.word()} · Actionable comments: {actionable}",
        ),
    )


def publish_deny(
    engine: Engine, meta: PublishMeta, reason: str, files: int, diff_bytes: int
) -> None:
    """Post the "too large to review" outcome: a marker-updated summary comment framed fail-like
    (🔴) plus a neutral check. No model call was made."""
    if already_published(engine, meta):
        engine.log.info(
            "deny already published for head SHA; skipping re-post repo=%s/%s sha=%s",
            meta.owner,
            meta.repo,
            meta.head_sha,
        )
        return
    marker = summary_marker(meta.owner, meta.repo, meta.number)
    body = (
        f"{marker}\n## 🔴 Agent review — too large for automated review\n\n"
        f"This PR is too large to review automatically ({files} files / {diff_bytes} bytes "
        "after excluding generated files). Please split it into smaller PRs.\n\n"
        f"_{reason}_\n"
    )
    engine.gh.upsert_marker_comment(meta.owner, meta.repo, meta.number, marker, body)
    engine.gh.create_check_run(
        meta.owner,
        meta.repo,
        CheckRunInput(
            name=CHECK_NAME,
            head_sha=meta.head_sha,
            conclusion="neutral",
            title="🔴 Agent review — too large",
            summary=f"{files} files / {diff_bytes} bytes after excluding generated files; "
            "please split.",
        ),
    )


def already_published(engine: Engine, meta: PublishMeta) -> bool:
    """Report whether the agent-review check already exists for the head SHA. A lookup error is
    treated as "not published" so a transient failure never suppresses a real review."""
    try:
        res = engine.gh.agent_check(meta.owner, meta.repo, meta.head_sha, CHECK_NAME)
    except Exception:  # noqa: BLE001
        return False
    return res.found


def classify(
    findings: list[Finding], idx: DiffIndex
) -> tuple[list[Finding], list[Finding], list[Finding]]:
    """Split confidence-gated findings into inline findings (actionable, on a commentable diff
    line), out-of-diff actionable findings (listed in the summary, never snapped to a wrong
    line), and nitpicks (collapsed in the summary)."""
    inline: list[Finding] = []
    out_of_diff: list[Finding] = []
    nitpicks: list[Finding] = []
    for f in findings:
        if f.severity is Severity.NITPICK:
            nitpicks.append(f)
            continue
        if f.file != "" and f.line > 0 and idx.in_diff(f.file, f.line):
            inline.append(f)
            continue
        out_of_diff.append(f)
    return inline, out_of_diff, nitpicks


def inline_comment_body(f: Finding) -> str:
    """Render one inline comment: an icon+category prefix, the message, an optional ```suggestion
    block (a localized fix), and an optional "Prompt for AI agents" block."""
    parts: list[str] = []
    # Dimension/severity are normalized to known enums, so only the model-authored message needs
    # sanitizing here.
    parts.append(f"**{finding_prefix(f)}** · _{f.dimension.value}_\n\n{sanitize_text(f.message)}\n")
    if f.suggestion != "":
        # Suggestion is model-authored; size the outer fence past any backtick run in it so a
        # suggestion containing a ```fence can't close the block early and inject markdown or
        # @mentions.
        fence = "`" * (max_backtick_run(f.suggestion) + 1)
        if len(fence) < 3:
            fence = "```"
        parts.append("\n" + fence + "suggestion\n")
        parts.append(f.suggestion)
        if not f.suggestion.endswith("\n"):
            parts.append("\n")
        parts.append(fence + "\n")
    if f.fix_prompt != "":
        # FixPrompt is model-authored; render it inside a code fence so any @mentions or HTML are
        # literal (not pinged/injected) and it stays copy-pasteable.
        fence = "`" * (max_backtick_run(f.fix_prompt) + 1)
        if len(fence) < 3:
            fence = "```"
        parts.append("\n<details>\n<summary>🤖 Prompt for AI agents</summary>\n\n")
        parts.append(fence + "\n")
        parts.append(f.fix_prompt)
        if not f.fix_prompt.endswith("\n"):
            parts.append("\n")
        parts.append(fence + "\n\n</details>\n")
    # Hidden fingerprint marker so a later re-review re-identifies this comment and reconciles it.
    parts.append("\n" + fp_marker(f.fingerprint()) + "\n")
    return "".join(parts)


# Matches an @ immediately followed by a mention character; sanitize_text inserts a zero-width
# space after the @ so GitHub does not render (and notify) it as a mention.
_MENTION_PATTERN = re.compile(r"@([A-Za-z0-9])")


def sanitize_text(s: str) -> str:
    """Neutralize model-authored text for safe embedding in a Markdown comment: escape
    HTML-significant characters (so a finding can't inject markup such as </details>) and break
    @mentions with a zero-width space (so the reviewer never pings a real user). Code in
    ```suggestion blocks and fenced FixPrompt is left untouched by callers."""
    s = s.replace("&", "&amp;")
    s = s.replace("<", "&lt;")
    s = s.replace(">", "&gt;")
    return _MENTION_PATTERN.sub("@​\\1", s)


def finding_prefix(f: Finding) -> str:
    """The icon+category label that leads an inline comment."""
    if f.dimension is Dimension.SECURITY:
        return "🔒 Security"
    if f.severity in (Severity.CRITICAL, Severity.MAJOR):
        return "⚠️ Potential issue"
    return "🛠️ Refactor"


def summary_comment(
    marker: str,
    card: Scorecard,
    actionable: int,
    nitpicks: list[Finding],
    out_of_diff: list[Finding],
    meta: PublishMeta,
) -> str:
    """Assemble the marker-updated summary comment: header, scorecard table, and collapsible
    sections for nitpicks, out-of-diff findings, and review details."""
    parts = [marker, "\n"]
    parts.append(
        f"## {card.overall.glyph()} Agent review — Overall: {card.overall.word()} · "
        f"Actionable comments: {actionable}\n\n"
    )
    parts.append(scorecard_table(card))
    if nitpicks:
        parts.append(collapsible(f"🧹 Nitpicks ({len(nitpicks)})", findings_list(nitpicks)))
    if out_of_diff:
        parts.append(
            collapsible(f"🔭 Outside diff range ({len(out_of_diff)})", findings_list(out_of_diff))
        )
    parts.append(collapsible("Review details", review_details(meta)))
    return "".join(parts)


def scorecard_table(card: Scorecard) -> str:
    """Render the per-dimension severity histogram. With no findings it states so rather than
    emitting an empty table."""
    if not card.dims:
        return "_No findings._\n\n"
    parts = [
        "| Dimension | Level | Critical | Major | Medium | Nitpick |\n",
        "|---|---|---|---|---|---|\n",
    ]
    for d in card.dims:
        parts.append(
            f"| {d.dimension.value} | {d.level.glyph()} | {d.critical} | {d.major} | "
            f"{d.medium} | {d.nitpick} |\n"
        )
    parts.append("\n")
    return "".join(parts)


def findings_list(fs: list[Finding]) -> str:
    """Render findings as a bulleted file:line list for the summary's collapsible sections."""
    parts: list[str] = []
    for f in fs:
        loc = f"{f.file}:{f.line}" if f.line > 0 else f.file
        parts.append(
            f"- **{f.severity.value}** `{loc}` _({f.dimension.value})_ — {sanitize_text(f.message)}\n"
        )
    return "".join(parts)


def review_details(meta: PublishMeta) -> str:
    """Render the "Review details" section: head SHA, file count, and the model tiers."""
    parts = [f"- Head SHA: `{meta.head_sha}`\n", f"- Files reviewed: {len(meta.files)}\n"]
    if meta.tiers != "":
        parts.append(f"- Model tiers: {meta.tiers}\n")
    if meta.standards:
        parts.append(f"- Standards applied: {', '.join(meta.standards)}\n")
    else:
        # Empty also covers standards-off and the discovery/distill fallback, not just a repo with
        # no convention docs — so stay neutral rather than asserting none were found.
        parts.append("- Standards: generic review\n")
    return "".join(parts)


def collapsible(summary: str, body: str) -> str:
    """Wrap ``body`` in a <details> block with the given summary label."""
    return f"\n<details>\n<summary>{summary}</summary>\n\n{body}\n</details>\n"


def check_conclusion(overall: Level) -> str:
    """Map the overall grade to the advisory check conclusion: green is success; yellow and red
    are neutral. It is never failure — the reviewer never gates a merge."""
    return "success" if overall is Level.GREEN else "neutral"
