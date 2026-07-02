"""The hidden fingerprint marker and the pure reconcile: given this run's inline findings + the
PR's existing comments, what to post vs minimize.

The marker tags each inline comment with the fingerprint of the finding that produced it, so a
later re-review re-identifies the comment from GitHub itself (GitHub-as-store — no local durable
state). It is an HTML comment appended to the body and is an external-ish contract: keep the
exact format stable across ports.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field

from automation_agent.agent.reviewer.findings import Finding
from automation_agent.githubapi import ReviewCommentRef

_FP_MARKER_PREFIX = "<!-- ar-fp:"
_FP_MARKER_SUFFIX = " -->"

# Extracts the fingerprint from a comment body. Non-greedy so a body with trailing content still
# matches only the marker payload.
_FP_MARKER_PATTERN = re.compile(r"<!-- ar-fp:(.+?) -->")


def fp_marker(fingerprint: str) -> str:
    """Render the hidden fingerprint marker appended to an inline comment body."""
    return _FP_MARKER_PREFIX + fingerprint + _FP_MARKER_SUFFIX


def parse_fp_marker(body: str) -> str:
    """Return the fingerprint embedded in a comment body, or "" if it carries none — a foreign
    comment, or one posted before reconciliation existed."""
    m = _FP_MARKER_PATTERN.search(body)
    return m.group(1) if m else ""


@dataclass
class ReconcileResult:
    """The outcome of comparing this run's inline findings against the comments already on the
    PR: which findings to post fresh, and which existing comments to minimize."""

    to_post: list[Finding] = field(default_factory=list)  # inline findings with no comment yet
    to_minimize: list[str] = field(
        default_factory=list
    )  # node ids of comments whose finding is gone


def reconcile(findings: list[Finding], existing: list[ReviewCommentRef]) -> ReconcileResult:
    """Compare this run's inline findings to the PR's existing fingerprinted review comments
    (GitHub-as-store). A finding already represented by a comment is kept — not re-posted, so a
    re-review is idempotent; a finding with no existing comment is posted; an existing
    fingerprinted comment with no matching finding this run is minimized as outdated. Comments
    without our marker (foreign, or pre-reconciliation) are ignored. ``to_minimize`` is sorted
    for deterministic behavior and tests."""
    current = {f.fingerprint() for f in findings}
    have: dict[str, list[str]] = {}  # fingerprint -> existing node ids
    for rc in existing:
        fp = parse_fp_marker(rc.body)
        if fp != "":
            have.setdefault(fp, []).append(rc.node_id)

    res = ReconcileResult()
    for f in findings:
        if f.fingerprint() not in have:
            res.to_post.append(f)
    for fp, ids in have.items():
        if fp not in current:
            res.to_minimize.extend(ids)
    res.to_minimize.sort()
    return res
