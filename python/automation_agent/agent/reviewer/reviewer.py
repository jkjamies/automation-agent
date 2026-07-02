"""The reviewer engine: dependencies, the intake pipeline (skip / deny / review), and the
Kickoff entry point.

Unlike the lint/coverage fixers, the reviewer is not a suspend/resume fix loop: it is mostly
one-shot per ``pull_request`` event and does not park on await_ci. Its long LLM compute runs
in-request via the execution transport (KindReview → /internal/dispatch), so CPU stays allocated
on Cloud Run.

The flow per pull_request event: parse it, apply the trigger and skip rules, fetch the changed
files via the REST API, filter generated/vendored churn, and apply the two-dimensional size gate
to reach a decision (skip / deny / review). A review fans out the category lenses + glue pass and
scores the findings (count-based scorecard). Publishing the scored review to the PR is a
follow-up.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from enum import Enum
from typing import Protocol, cast

from google.adk.models import BaseLlm

from automation_agent.agent.reviewer.filter import FileFilter
from automation_agent.agent.reviewer.findings import Finding, clamp_threshold
from automation_agent.agent.reviewer.review import run_review
from automation_agent.agent.reviewer.sizegate import oversize
from automation_agent.githubapi import (
    PRFile,
    PullRequestEvent,
    parse_pull_request_event,
)

# Marks branches the fixers create (they push to automation-agent/...). The reviewer skips PRs
# from these branches so it never reviews the fixers' own PRs in a loop. Mirrors the
# AGENT_PR_LABEL namespace.
OWN_BRANCH_PREFIX = "automation-agent/"


class GitHubClient(Protocol):
    """The slice of ``githubapi.Client`` the reviewer needs to detect and analyze a PR: read the
    changed files (with patches) and read the head SHA (to detect a task superseded by a newer
    push). A local protocol keeps the engine testable with a fake."""

    def list_pr_files(self, owner: str, repo: str, number: int) -> list[PRFile]: ...
    def pull_request_head_sha(self, owner: str, repo: str, number: int) -> str: ...


@dataclass
class Deps:
    """Wires the reviewer engine."""

    # enabled is the REVIEW_ENABLED kill switch. When false the engine accepts and acknowledges
    # pull_request events but does no review work — the default and the rollback posture.
    enabled: bool = False
    gh: GitHubClient | None = None
    base_llm: BaseLlm | None = None
    code_llm: BaseLlm | None = None
    # min_confidence drops findings below this confidence before scoring (the phase-1 verify
    # gate). A non-positive value keeps everything.
    min_confidence: float = 0.0
    # skip_drafts skips draft PRs unless the triggering action is ready_for_review.
    skip_drafts: bool = True
    # exclude_globs drops generated/vendored/lockfile/minified/binary paths before sizing.
    exclude_globs: list[str] = field(default_factory=list)
    # max_files / max_diff_bytes are the two-dimensional size-gate caps; a non-positive value
    # disables that dimension.
    max_files: int = 0
    max_diff_bytes: int = 0
    # standards_enabled toggles standards-aware review: discover the reviewed repo's own
    # convention docs, distill them, and steer the lenses off them.
    standards_enabled: bool = False
    standards_globs: list[str] = field(default_factory=list)
    standards_max_bytes: int = 0
    # uncited_drop, when true (REVIEW_UNCITED_MODE=drop), drops a conformance finding that cites
    # no real repo rule; otherwise (default) it is demoted to nitpick.
    uncited_drop: bool = False
    log: logging.Logger | None = None


class DecisionKind(Enum):
    """The outcome of intake for one pull_request event."""

    SKIP = 0  # not reviewable (trigger/skip rule or empty diff)
    DENY = 1  # reviewable but too large — deny, don't degrade
    REVIEW = 2  # proceed to review the kept files


@dataclass
class Decision:
    """The result of the intake pipeline. files/diff_bytes are the filtered review surface (set
    for deny and review); reason explains a skip or a deny."""

    kind: DecisionKind
    reason: str = ""
    files: list[PRFile] = field(default_factory=list)
    diff_bytes: int = 0


class Engine:
    """Runs the PR code-review workflow for one pull_request event."""

    def __init__(self, d: Deps) -> None:
        self.enabled = d.enabled
        # gh / base_llm / code_llm are required for real work; kickoff guards each with a
        # controlled error before any use (disabled/skip/deny paths never touch the missing one),
        # so the collaborators can treat them as always-present.
        self.gh = cast("GitHubClient", d.gh)
        self.base_llm = cast("BaseLlm", d.base_llm)
        self.code_llm = cast("BaseLlm", d.code_llm)
        self.min_confidence = clamp_threshold(d.min_confidence)
        self.skip_drafts = d.skip_drafts
        self.filter = FileFilter(d.exclude_globs)
        self.max_files = d.max_files
        self.max_diff_bytes = d.max_diff_bytes
        self.standards_enabled = d.standards_enabled
        self.standards_globs = d.standards_globs
        self.standards_max_bytes = d.standards_max_bytes
        self.uncited_drop = d.uncited_drop
        self.log = d.log if d.log is not None else logging.getLogger("automation_agent")

    async def kickoff(self, raw: bytes) -> None:
        """Handle one pull_request webhook delivery (KindReview). The root dispatcher calls it
        with the raw event payload; it runs in-request via the execution transport.

        When disabled (REVIEW_ENABLED=false, the default) it no-ops, so the feature is dark by
        default and REVIEW_ENABLED is the kill switch. When enabled it runs intake and either
        skips, denies (too large), or publishes a scored review.
        """
        if not self.enabled:
            self.log.debug(
                "reviewer disabled (REVIEW_ENABLED=false); ignoring pull_request event bytes=%d",
                len(raw),
            )
            return
        # An enabled engine needs a client to fetch the diff and publish (both deny and review use
        # it); without it, raise a controlled error rather than dereferencing a nil dependency.
        if self.gh is None:
            raise ValueError("reviewer: enabled but GitHub client not configured")
        try:
            ev = parse_pull_request_event(raw)
        except ValueError as exc:
            raise ValueError(f"reviewer: {exc}") from exc
        d = self.decide(ev)
        pr = f"{ev.repo_full_name}#{ev.number}"
        # decide() already validated the full name before reaching a deny/review decision, so a
        # malformed name here means skip.
        owner, repo, _ = split_full_name(ev.repo_full_name)
        # Coalesce-to-latest: a deny/review acts on the event's SHA, so if a newer push has
        # superseded it, skip rather than produce a stale review. A skip produced nothing.
        if d.kind is not DecisionKind.SKIP and self._superseded(owner, repo, ev):
            self.log.info(
                "stale review skipped (superseded by a newer push) pr=%s event_sha=%s",
                pr,
                ev.head_sha,
            )
            return
        if d.kind is DecisionKind.SKIP:
            self.log.info("review skipped pr=%s action=%s reason=%s", pr, ev.action, d.reason)
        elif d.kind is DecisionKind.DENY:
            # Too large to review: it is denied, not degraded. Publishing the "please split"
            # notice is a follow-up.
            self.log.info(
                "review denied pr=%s files=%d diff_bytes=%d reason=%s",
                pr,
                len(d.files),
                d.diff_bytes,
                d.reason,
            )
        else:  # DecisionKind.REVIEW
            # Review needs both tier models; the deny branch above does not.
            if self.base_llm is None or self.code_llm is None:
                raise ValueError("reviewer: enabled but review models not configured")
            card, findings = await run_review(self, d.files)
            # Publishing the scored review to the PR is a follow-up.
            self.log.info(
                "review scored pr=%s files=%d overall=%s findings=%d",
                pr,
                len(d.files),
                card.overall.glyph(),
                card.total,
            )

    def decide(self, ev: PullRequestEvent) -> Decision:
        """Run the deterministic intake pipeline for one event: trigger gate → skip rules → fetch
        files → filter → size gate. It performs no model calls and posts nothing."""
        if ev.action not in ("opened", "reopened", "synchronize", "ready_for_review"):
            return _skip(f'action "{ev.action}" is not a reviewed trigger')
        if self.skip_drafts and ev.draft and ev.action != "ready_for_review":
            return _skip("draft PR (REVIEW_SKIP_DRAFTS)")
        if ev.head_ref.startswith(OWN_BRANCH_PREFIX):
            return _skip(f'agent\'s own PR (head "{ev.head_ref}")')
        if "skip-review" in ev.labels:
            return _skip("skip-review label")
        if is_dependency_bot(ev.author_login):
            return _skip(f"dependency-bot PR ({ev.author_login})")

        owner, repo, ok = split_full_name(ev.repo_full_name)
        if not ok:
            raise ValueError(f'reviewer: malformed repository full name "{ev.repo_full_name}"')
        assert self.gh is not None  # kickoff guarantees a client before decide runs
        try:
            files = self.gh.list_pr_files(owner, repo, ev.number)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"reviewer: list PR files: {exc}") from exc
        kept, diff_bytes = self.filter.apply(files)
        if not kept:
            return _skip(f"no reviewable files after exclude filter ({len(files)} changed)")
        reason, denied = oversize(len(kept), diff_bytes, self.max_files, self.max_diff_bytes)
        if denied:
            return Decision(
                kind=DecisionKind.DENY, reason=reason, files=kept, diff_bytes=diff_bytes
            )
        return Decision(kind=DecisionKind.REVIEW, files=kept, diff_bytes=diff_bytes)

    def _superseded(self, owner: str, repo: str, ev: PullRequestEvent) -> bool:
        """Report whether a newer push has replaced the SHA this task was enqueued for. It is
        best-effort: a missing event SHA or a lookup error yields False (proceed) so a transient
        failure never suppresses a real review."""
        if ev.head_sha == "":
            return False
        assert self.gh is not None
        try:
            current = self.gh.pull_request_head_sha(owner, repo, ev.number)
        except Exception as exc:  # noqa: BLE001
            self.log.warning(
                "could not fetch current head SHA; proceeding with review pr=%s err=%s",
                ev.repo_full_name,
                exc,
            )
            return False
        return current != "" and current != ev.head_sha


def new_engine(d: Deps) -> Engine:
    """Build the reviewer engine from its dependencies."""
    return Engine(d)


def _skip(reason: str) -> Decision:
    """Build a skip decision with a formatted reason."""
    return Decision(kind=DecisionKind.SKIP, reason=reason)


def is_dependency_bot(login: str) -> bool:
    """Report whether the author is a known dependency-update bot. GitHub Apps post as
    "<name>[bot]"."""
    return login in ("dependabot[bot]", "renovate[bot]")


def split_full_name(full: str) -> tuple[str, str, bool]:
    """Split an "owner/name" repository full name. Reports False for anything that is not exactly
    one owner and one non-empty name."""
    owner, sep, repo = full.partition("/")
    if not sep or owner == "" or repo == "" or "/" in repo:
        return "", "", False
    return owner, repo, True


# Re-exported so the engine's collaborators keep a single import site for Finding.
__all__ = [
    "Decision",
    "DecisionKind",
    "Deps",
    "Engine",
    "Finding",
    "GitHubClient",
    "is_dependency_bot",
    "new_engine",
    "split_full_name",
]
