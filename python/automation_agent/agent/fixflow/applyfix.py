"""The apply mechanics: checkout, write edits, commit, push, ensure a labeled PR.

Port of ``fixflow/applyfix.go``. :func:`open_repo` clones into a fresh temp dir and
checks out the agent branch (created from base on kickoff, the existing remote branch
on retry). :func:`commit` writes the edits path-safely, commits, pushes, and ensures a
labeled PR exists. :func:`apply_fix` does both in one step (a test convenience).
"""

from __future__ import annotations

import os
import tempfile
from dataclasses import dataclass
from typing import Protocol

from automation_agent.agent.fixflow.files import _safe_join
from automation_agent.githubapi import PR, PRInput
from automation_agent.gitrepo import Author, Repo


class GitHub(Protocol):
    """The slice of githubapi the apply step needs (consumer-defined, fakeable)."""

    def find_agent_prs(self, owner: str, repo: str, label: str) -> list[PR]: ...

    def create_pr(self, owner: str, repo: str, in_: PRInput) -> PR: ...

    def add_labels(self, owner: str, repo: str, number: int, *labels: str) -> None: ...


@dataclass
class FileEdit:
    """A whole-file write an analyze step produces (a rewritten source file, a
    generated test file, …)."""

    path: str  # repo-relative path
    content: str


@dataclass
class ApplyConfig:
    """Parameterizes one apply."""

    owner: str
    repo: str
    clone_url: str
    token: str
    base: str  # base branch the PR targets
    branch: str  # agent working branch
    new_branch: bool  # True on kickoff (create from base); False on retry (reuse)
    label: str
    commit_message: str
    pr_title: str
    pr_body: str
    author: Author


@dataclass
class ApplyResult:
    """The outcome of one apply."""

    pr: PR
    head_sha: str


def open_repo(cfg: ApplyConfig) -> Repo:
    """Clone the repo into a fresh temp dir and check out the agent branch. The caller
    must remove ``repo.dir()`` when done."""
    dir_ = tempfile.mkdtemp(prefix="agentfix-")
    # gitrepo.Repo.clone requires the target dir not to already exist.
    os.rmdir(dir_)
    try:
        repo = Repo.clone(cfg.clone_url, dir_, cfg.token)
    except Exception:
        _rmtree(dir_)
        raise
    try:
        if cfg.new_branch:
            repo.checkout(cfg.branch, create=True)
        else:
            repo.checkout_remote(cfg.branch)
    except Exception:
        _rmtree(repo.dir())
        raise
    return repo


def commit(gh: GitHub, repo: Repo, cfg: ApplyConfig, edits: list[FileEdit]) -> ApplyResult:
    """Write edits into the working tree, commit, push, and ensure a labeled PR exists."""
    if not edits:
        raise ValueError("apply: no edits to apply")
    _write_edits(repo, edits)
    sha = repo.commit_all(cfg.commit_message, cfg.author)
    repo.push()
    pr = _ensure_pr(gh, cfg)
    return ApplyResult(pr=pr, head_sha=sha)


def apply_fix(gh: GitHub, cfg: ApplyConfig, edits: list[FileEdit]) -> ApplyResult:
    """Open a checkout and commit edits in one step — a convenience used in tests; the
    engine interleaves analysis between :func:`open_repo` and :func:`commit`."""
    repo = open_repo(cfg)
    try:
        return commit(gh, repo, cfg, edits)
    finally:
        _rmtree(repo.dir())


def _write_edits(repo: Repo, edits: list[FileEdit]) -> None:
    for e in edits:
        # Reject LLM-controlled paths that escape the checkout (path traversal).
        full = _safe_join(repo.dir(), e.path)
        os.makedirs(os.path.dirname(full), exist_ok=True)
        with open(full, "w", encoding="utf-8") as f:
            f.write(e.content)


def _ensure_pr(gh: GitHub, cfg: ApplyConfig) -> PR:
    """Return the existing agent PR for the branch, or create and label one."""
    existing = gh.find_agent_prs(cfg.owner, cfg.repo, cfg.label)
    for pr in existing:
        if pr.branch == cfg.branch:
            return pr
    pr = gh.create_pr(
        cfg.owner,
        cfg.repo,
        PRInput(title=cfg.pr_title, head=cfg.branch, base=cfg.base, body=cfg.pr_body),
    )
    gh.add_labels(cfg.owner, cfg.repo, pr.number, cfg.label)
    pr.labels = list(pr.labels) + [cfg.label]
    return pr


def _rmtree(path: str) -> None:
    import shutil

    shutil.rmtree(path, ignore_errors=True)
