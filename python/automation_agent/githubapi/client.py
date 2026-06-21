"""Thin wrapper over PyGithub exposing the narrow operations this service needs:
reading recent commits, opening/labeling/finding agent PRs, counting attempts, and
reading the agent verify check.

Deterministic tooling — no agent imports (an arch test enforces this).

Functions return a value and RAISE on error. PyGithub is synchronous, so there
is no request-context parameter to plumb through.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from github import Auth, Github


@dataclass
class Commit:
    """Minimal commit projection for digests."""

    sha: str
    message: str
    author: str
    url: str
    when: datetime | None


@dataclass
class PR:
    """Minimal pull-request projection."""

    number: int
    title: str
    branch: str
    head_sha: str
    url: str
    labels: list[str] = field(default_factory=list)


@dataclass
class PRInput:
    """Describes a pull request to open."""

    title: str
    head: str  # source branch
    base: str  # target branch
    body: str = ""


@dataclass
class CheckResult:
    """The agent verify check's state for a ref."""

    found: bool
    name: str = ""
    status: str = ""  # queued | in_progress | completed
    conclusion: str = ""  # success | failure | ... (when completed)
    output_text: str = ""  # the check's output (lint findings), used to re-triage on resume
    started_at: datetime | None = None
    completed_at: datetime | None = None


@dataclass
class CheckEvent:
    """The parsed essentials of a GitHub check_run webhook event."""

    action: str = ""  # created | completed | rerequested
    check_name: str = ""
    status: str = ""  # queued | in_progress | completed
    conclusion: str = ""  # success | failure | ... (when completed)
    head_sha: str = ""
    pr_number: int = 0
    pr_branch: str = ""
    repo_full_name: str = ""  # owner/name
    output_text: str = ""  # the check's output (lint findings), used to re-triage on resume


class Client:
    """A thin wrapper over a PyGithub ``Github`` instance. Owner/repo are passed
    per call so one client serves many repositories.
    """

    def __init__(self, token: str = "") -> None:
        """Build a Client. An empty token yields an unauthenticated client (fine
        for public reads and tests).
        """
        if token:
            self._gh = Github(auth=Auth.Token(token))
        else:
            self._gh = Github()

    def list_commits_since(self, owner: str, repo: str, since: datetime) -> list[Commit]:
        """Return commits to owner/repo authored since the given time."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            commits = r.get_commits(since=since)
            return [_to_commit(rc) for rc in commits]
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"list commits {owner}/{repo}: {exc}") from exc

    def create_pr(self, owner: str, repo: str, in_: PRInput) -> PR:
        """Open a pull request."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            pr = r.create_pull(title=in_.title, head=in_.head, base=in_.base, body=in_.body)
            return _to_pr(pr)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"create PR {owner}/{repo}: {exc}") from exc

    def add_labels(self, owner: str, repo: str, number: int, *labels: str) -> None:
        """Add labels to a PR (PRs are issues for the labels API)."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            issue = r.get_issue(number)
            issue.add_to_labels(*labels)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"add labels to {owner}/{repo}#{number}: {exc}") from exc

    def find_agent_prs(self, owner: str, repo: str, label: str) -> list[PR]:
        """List open PRs carrying the given label."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            prs = r.get_pulls(state="open")
            out: list[PR] = []
            for pr in prs:
                p = _to_pr(pr)
                if label in p.labels:
                    out.append(p)
            return out
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"list PRs {owner}/{repo}: {exc}") from exc

    def attempt_count(self, owner: str, repo: str, number: int) -> int:
        """Return the number of commits on a PR.

        With the invariant that the agent pushes exactly one commit per attempt,
        this equals the distinct agent-pushed head SHAs — re-run-safe, since a
        manual check re-run adds no commit.
        """
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            pr = r.get_pull(number)
            return pr.get_commits().totalCount
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"list PR commits {owner}/{repo}#{number}: {exc}") from exc

    def agent_check(self, owner: str, repo: str, ref: str, check_name: str) -> CheckResult:
        """Return the named check's state for ref, or ``CheckResult(found=False)``
        if absent.
        """
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            commit = r.get_commit(ref)
            runs = commit.get_check_runs(check_name=check_name)
            # Guard on both the count and the actual page: a positive total with an
            # empty first page would otherwise IndexError on runs[0].
            if runs.totalCount == 0:
                return CheckResult(found=False)
            try:
                cr = runs[0]
            except IndexError:
                return CheckResult(found=False)
            out = CheckResult(
                found=True,
                name=cr.name or "",
                status=cr.status or "",
                conclusion=cr.conclusion or "",
                started_at=cr.started_at,
                completed_at=cr.completed_at,
            )
            output = cr.output
            if output is not None:
                text = output.text or ""
                if text == "":
                    text = output.summary or ""
                out.output_text = text
            return out
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"list check runs {owner}/{repo}@{ref}: {exc}") from exc

    def get_file_content(self, owner: str, repo: str, path: str, ref: str = "") -> str:
        """Return the decoded contents of a file at ref (ref may be ``""`` for the
        default branch).
        """
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            fc = r.get_contents(path, ref=ref) if ref else r.get_contents(path)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"get {owner}/{repo}:{path}: {exc}") from exc
        if isinstance(fc, list):
            raise ValueError(f"{path} is a directory, not a file")
        try:
            return fc.decoded_content.decode("utf-8")
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"decode {path}: {exc}") from exc


def parse_check_run_event(body: bytes) -> CheckEvent:
    """Parse a check_run webhook body into a :class:`CheckEvent`.

    Missing fields degrade to empty/0 defaults.
    """
    try:
        ev: dict[str, Any] = json.loads(body)
    except Exception as exc:  # noqa: BLE001
        raise ValueError(f"parse check_run event: {exc}") from exc

    cr = ev.get("check_run") or {}
    out = CheckEvent(
        action=ev.get("action") or "",
        check_name=cr.get("name") or "",
        status=cr.get("status") or "",
        conclusion=cr.get("conclusion") or "",
        head_sha=cr.get("head_sha") or "",
        repo_full_name=((ev.get("repository") or {}).get("full_name")) or "",
    )
    prs = cr.get("pull_requests") or []
    if prs:
        first = prs[0] or {}
        out.pr_number = first.get("number") or 0
        out.pr_branch = ((first.get("head") or {}).get("ref")) or ""
    output = cr.get("output")
    if output is not None:
        text = output.get("text") or ""
        if text == "":
            text = output.get("summary") or ""
        out.output_text = text
    return out


def _to_commit(rc: Any) -> Commit:
    c = rc.commit
    author = c.author
    return Commit(
        sha=rc.sha or "",
        message=(c.message or "") if c is not None else "",
        author=(author.name or "") if author is not None else "",
        url=rc.html_url or "",
        when=author.date if author is not None else None,
    )


def _to_pr(pr: Any) -> PR:
    labels = [label.name for label in (pr.labels or [])]
    head = pr.head
    return PR(
        number=pr.number or 0,
        title=pr.title or "",
        branch=(head.ref or "") if head is not None else "",
        head_sha=(head.sha or "") if head is not None else "",
        url=pr.html_url or "",
        labels=labels,
    )
