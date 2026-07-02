"""Thin wrapper over PyGithub exposing the narrow operations this service needs:
reading recent commits, opening/labeling PRs, finding the open PR for a branch,
counting attempts, and reading the agent verify check.

Deterministic tooling — no agent imports (an arch test enforces this).

Functions return a value and RAISE on error. PyGithub is synchronous, so there
is no request-context parameter to plumb through.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Protocol

from github import Github


class AuthProvider(Protocol):
    """The slice of ``auth.TokenProvider`` this client needs: a ready PyGithub REST
    client carrying the right credentials (a static PAT, an anonymous client, or
    auto-refreshed GitHub App installation tokens). Declared locally so githubapi stays
    decoupled from the ``auth`` package (structural typing matches the real providers)."""

    def github(self) -> Github: ...


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
class ChangedFile:
    """One file changed in a comparison (base...head diff)."""

    path: str
    status: str = ""  # added | modified | removed | renamed
    additions: int = 0
    deletions: int = 0


@dataclass
class Comparison:
    """A base...head comparison: the commits and files a PR branch added."""

    total_commits: int = 0
    files: list[ChangedFile] = field(default_factory=list)


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
class PRFile:
    """One changed file in a pull request: its path, change status, line counts, and the
    unified diff patch. ``patch`` carries the hunk text the reviewer needs to map a finding to
    a diff line; GitHub omits it for binary or very large files, so it is then empty — kept,
    not an error. Because an empty patch is ambiguous (binary vs. oversized text),
    ``additions``/``deletions`` are reported even when the patch is omitted, letting an omitted
    text diff be charged conservatively from its line counts rather than as zero diff bytes."""

    path: str
    previous_path: str = ""  # prior path for a rename, else empty
    status: str = ""  # added | modified | removed | renamed | copied | changed
    additions: int = 0
    deletions: int = 0
    patch: str = ""  # unified diff hunks; empty for binary/oversized files


@dataclass
class PullRequestEvent:
    """The parsed essentials of a GitHub pull_request webhook event — the reviewer's
    native-event kickoff. The diff itself is fetched separately via :meth:`Client.list_pr_files`
    (the event body carries only metadata)."""

    action: str = ""  # opened | reopened | synchronize | ready_for_review | ...
    number: int = 0
    repo_full_name: str = ""  # owner/name
    head_ref: str = ""  # source branch
    head_sha: str = ""
    base_ref: str = ""  # target branch
    draft: bool = False
    labels: list[str] = field(default_factory=list)
    author_login: str = ""  # PR author login (e.g. "dependabot[bot]")


@dataclass
class TreeEntry:
    """One entry in a repository git tree: its repo-relative path, blob/tree SHA, and type."""

    path: str
    sha: str = ""
    type: str = ""  # "blob" | "tree"


@dataclass
class ReviewCommentRef:
    """Identifies an existing inline review comment for reconciliation: its GraphQL node id (the
    minimize_comment subject) and its body (which carries the hidden fingerprint marker)."""

    node_id: str
    body: str


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

    def __init__(
        self,
        provider: AuthProvider,
        *,
        authored_login: str = "",
        app_authored: bool = False,
    ) -> None:
        """Build a Client from an auth provider (``auth.StaticProvider`` for PAT /
        anonymous, ``auth.AppProvider`` for GitHub App installation tokens). The provider
        owns the underlying PyGithub client and its token refresh.

        ``authored_login`` is the login this client authors content as; when known,
        :meth:`upsert_marker_comment` edits only comments by this login — the authoritative
        ownership signal. ``app_authored`` is the fallback when the login is unknown: an App
        installation posts as a bot user (type ``"Bot"``), so an in-place edit is restricted to
        bot-authored comments rather than any comment echoing the marker.
        """
        self._gh = provider.github()
        self._authored_login = authored_login
        self._app_authored = app_authored

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

    def find_open_pr_by_branch(self, owner: str, repo: str, branch: str) -> PR | None:
        """Return the open PR whose head is the given branch, or ``None``. Lookup is by
        branch (the GitHub ``head=owner:branch`` filter), not the agent label — the label
        is write-only, applied on creation for humans to filter on."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            prs = r.get_pulls(state="open", head=f"{owner}:{branch}")
            for pr in prs:
                return _to_pr(pr)
            return None
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"list PRs {owner}/{repo} head {branch}: {exc}") from exc

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

    def compare(self, owner: str, repo: str, base: str, head: str) -> Comparison:
        """Return the base...head comparison (commit count + changed files). Used to
        enrich a terminal summary with what the agent actually changed on the PR."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            cmp = r.compare(base, head)
            files = [
                ChangedFile(
                    path=f.filename or "",
                    status=f.status or "",
                    additions=f.additions or 0,
                    deletions=f.deletions or 0,
                )
                for f in (cmp.files or [])
            ]
            return Comparison(total_commits=cmp.total_commits or 0, files=files)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"compare {owner}/{repo} {base}...{head}: {exc}") from exc

    def agent_check(self, owner: str, repo: str, ref: str, check_name: str) -> CheckResult:
        """Return the named check's state for ref, or ``CheckResult(found=False)``
        if absent.
        """
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            commit = r.get_commit(ref)
            # filter="latest": on a re-run, return only the most recent run per check.
            runs = commit.get_check_runs(check_name=check_name, filter="latest")
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

    def list_pr_files(self, owner: str, repo: str, number: int) -> list[PRFile]:
        """Return every changed file in a pull request (following pagination). It is the
        reviewer's primary input — changed files + patches — fetched via REST."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            pr = r.get_pull(number)
            return [
                PRFile(
                    path=f.filename or "",
                    previous_path=f.previous_filename or "",
                    status=f.status or "",
                    additions=f.additions or 0,
                    deletions=f.deletions or 0,
                    patch=f.patch or "",
                )
                for f in pr.get_files()
            ]
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"list PR files {owner}/{repo}#{number}: {exc}") from exc

    def pull_request_head_sha(self, owner: str, repo: str, number: int) -> str:
        """Return the PR's current head commit SHA. The reviewer compares it to the SHA carried
        by a review task to detect a task superseded by a newer push and skip a stale review."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            pr = r.get_pull(number)
            head = pr.head
            return (head.sha or "") if head is not None else ""
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"get PR {owner}/{repo}#{number}: {exc}") from exc

    def list_review_comments(self, owner: str, repo: str, number: int) -> list[ReviewCommentRef]:
        """Return the PR's inline review comments (paginated). Reconciliation parses the
        fingerprint marker from each body to decide what to keep, add, or minimize."""
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            pr = r.get_pull(number)
            return [
                ReviewCommentRef(node_id=rc.node_id or "", body=rc.body or "")
                for rc in pr.get_review_comments()
            ]
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"list review comments {owner}/{repo}#{number}: {exc}") from exc

    def tree(self, owner: str, repo: str, ref: str) -> tuple[list[TreeEntry], bool]:
        """List the repository's git tree at ``ref`` (a commit SHA, branch, or tag),
        recursively — how the reviewer discovers a repo's own standards docs without a clone.

        The second return is GitHub's truncation flag: the API caps a recursive tree (very large
        repos), and a truncated listing may omit entries, so the caller can decide whether
        incomplete discovery is acceptable rather than silently missing files.
        """
        try:
            r = self._gh.get_repo(f"{owner}/{repo}")
            t = r.get_git_tree(ref, recursive=True)
            entries = [
                TreeEntry(path=te.path or "", sha=te.sha or "", type=te.type or "")
                for te in (t.tree or [])
            ]
            return entries, bool(t.truncated)
        except Exception as exc:  # noqa: BLE001
            raise ValueError(f"get tree {owner}/{repo}@{ref}: {exc}") from exc


def parse_pull_request_event(body: bytes) -> PullRequestEvent:
    """Parse a pull_request webhook body into the fields the reviewer gates on. It mirrors
    :func:`parse_check_run_event`: the webhook JSON is decoded in the tooling layer so the agent
    consumes a stable projection, never the raw SDK type.

    Raises:
        ValueError: if the body is not valid JSON.
    """
    try:
        ev: dict[str, Any] = json.loads(body)
    except Exception as exc:  # noqa: BLE001
        raise ValueError(f"parse pull_request event: {exc}") from exc

    pr = ev.get("pull_request") or {}
    head = pr.get("head") or {}
    base = pr.get("base") or {}
    out = PullRequestEvent(
        action=ev.get("action") or "",
        number=pr.get("number") or 0,
        repo_full_name=((ev.get("repository") or {}).get("full_name")) or "",
        head_ref=head.get("ref") or "",
        head_sha=head.get("sha") or "",
        base_ref=base.get("ref") or "",
        draft=bool(pr.get("draft")),
        author_login=((pr.get("user") or {}).get("login")) or "",
    )
    for label in pr.get("labels") or []:
        name = (label or {}).get("name")
        if name:
            out.labels.append(name)
    return out


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
