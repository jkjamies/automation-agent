"""The reusable engine behind the PR-fixing agents (lint-fixer, coverage-fixer, …).

It owns the event-driven loop — kickoff -> suspend ->
CI resume -> loop or finish — plus the apply mechanics and attempt counting. Each
concrete agent supplies a :class:`Spec` (triage fn, analyze fn, branch + check
name). The durable artifacts live on GitHub; the CI-wait suspend/resume itself is owned
by the :class:`Driver` (ADK long-running + an injected setup.ParkStore backend).
"""

from __future__ import annotations

import logging
import shutil
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from datetime import timedelta

from google.adk.models import BaseLlm
from google.adk.sessions import BaseSessionService

from automation_agent.agent.fixflow.applyfix import (
    ApplyConfig,
    ApplyResult,
    FileEdit,
    GitHub,
    commit,
    open_repo,
)
from automation_agent.agent.fixflow.envelope import parse_kickoff
from automation_agent.agent.setup import ParkStore
from automation_agent.githubapi import parse_check_run_event
from automation_agent.gitrepo import Author
from automation_agent.notify import Message, Notifier


@dataclass
class FileWork:
    """One file and the items to address in it (lint problems, uncovered regions, …) —
    the normalized output of a Spec's triage step."""

    path: str
    items: list[str] = field(default_factory=list)


# TriageFunc normalizes an arbitrary tool report into per-file work (LLM-backed).
TriageFunc = Callable[[BaseLlm, str], Awaitable[list[FileWork]]]


@dataclass
class AnalyzeInput:
    """What an AnalyzeFunc receives. ``repo_dir`` is the checked-out working tree:
    analyze reads source from it (and may explore it), and the engine commits whatever
    edits are returned from the same checkout. ``llm`` is the default (planning) model;
    ``code_llm`` is the (often larger) model for writing code."""

    llm: BaseLlm
    code_llm: BaseLlm | None
    repo_dir: str
    work: list[FileWork]
    feedback: str = ""  # previous attempt's CI failure, on retry

    def coder(self) -> BaseLlm:
        """Return the code-change model, falling back to the default when none is set."""
        return self.code_llm if self.code_llm is not None else self.llm


# AnalyzeFunc produces the whole-file edits to apply (rewritten source, new tests, …).
AnalyzeFunc = Callable[[AnalyzeInput], Awaitable[list[FileEdit]]]


@dataclass
class Spec:
    """Per-workflow configuration that turns the engine into a concrete fixing agent."""

    name: str  # "lint" | "coverage"
    branch: str  # e.g. automation-agent/lint-fix
    check_name: str  # e.g. agent-lint-verify
    commit_message: str
    pr_title: str
    success_title: str  # notification title on success
    review_title: str  # notification title when human review is needed
    triage: TriageFunc
    analyze: AnalyzeFunc


def _default_clone_url(owner: str, repo: str) -> str:
    return f"https://github.com/{owner}/{repo}.git"


@dataclass
class Deps:
    """Runtime dependencies shared by all engines. ``code_llm`` is the model for the
    code-change steps (typically larger); it falls back to ``llm`` when None.
    ``ci_timeout`` bounds how long a suspended run waits for its CI result."""

    llm: BaseLlm | None = None
    code_llm: BaseLlm | None = None
    gh: GitHub | None = None
    notify: Notifier | None = None
    token: str = ""
    # pr_label is the single human-facing label applied to every agent PR on creation
    # (AGENT_PR_LABEL). Write-only — PR lookup is by branch, so it never gates behavior.
    pr_label: str = "automation-agent"
    # repos is the kickoff allowlist (REPOS). When non-empty, a kickoff whose repo is not
    # listed is rejected; empty imposes no restriction (restriction is opt-in).
    repos: list[str] = field(default_factory=list)
    max_iter: int = 3
    ci_timeout: timedelta = field(default_factory=lambda: timedelta(minutes=90))
    author: Author = field(
        default_factory=lambda: Author(
            name="automation-agent",
            email="automation-agent@users.noreply.github.com",
        )
    )
    log: logging.Logger | None = None
    # git_transport selects the clone-URL scheme the default builder uses: "https" (default
    # — token / GitHub App) or "ssh" (local dev — ssh-agent/keys). A test-injected
    # ``clone_url`` overrides it.
    git_transport: str = "https"
    # ssh_key is the explicit private-key path used when git_transport is "ssh" (GIT_SSH_KEY);
    # empty falls back to ssh-agent then default identities. Ignored for https.
    ssh_key: str = ""
    # clone_url, when set, overrides the built-in transport-based URL builder (tests inject a
    # local seed-repo path). None uses the default https/ssh builder keyed on git_transport.
    clone_url: Callable[[str, str], str] | None = None
    # session_service stores the durable suspend/resume history for the parked fix loop.
    # None falls back to in-memory (a restart strands parked runs); a durable backend
    # (sqlite/firestore) lets a parked run resume after a restart. Built once at startup.
    session_service: BaseSessionService | None = None
    # park_store persists the park record (pr_key -> session, attempts, run params) so a
    # resume — and, with a durable backend, a restart — can reconstruct it. None falls back
    # to the in-memory store. Built once at startup, alongside session_service.
    park_store: ParkStore | None = None


@dataclass
class ResumeInput:
    """The normalized resume context derived from a check_run webhook. The parked run
    already holds owner/repo/branch from kickoff, so resume only needs the PR identity,
    the conclusion, and the CI output (used as retry feedback)."""

    full_repo: str
    pr_number: int
    conclusion: str
    output_text: str


class Engine:
    """Runs one Spec's event-driven fix loop."""

    def __init__(self, spec: Spec, d: Deps) -> None:
        self.spec = spec
        self.d = d
        # Imported here to avoid a module import cycle (driver references Engine).
        from automation_agent.agent.fixflow.driver import Driver

        self.driver = Driver(self)

    def label(self) -> str:
        """The human-facing label applied to this engine's PRs (AGENT_PR_LABEL). Same for
        every workflow and write-only — PR lookup is by branch, not label."""
        return self.d.pr_label

    def check_name(self) -> str:
        """The agent verify check this engine resumes on."""
        return self.spec.check_name

    async def sweep_timeouts(self) -> None:
        """Resolve this engine's parked runs whose CI never reported — the durable timeout
        catch-all driven by Cloud Scheduler via ``/internal/sweep``."""
        await self.driver.sweep_timeouts()

    async def kickoff(self, raw: bytes) -> None:
        """Handle a kickoff envelope: start a suspended fix run (apply -> await CI)."""
        k = parse_kickoff(raw)
        if not self._repo_allowed(k.repo):
            if self.d.log is not None:
                self.d.log.warning(
                    "fix kickoff rejected: repo not in allowlist",
                    extra={"workflow": self.spec.name, "repo": k.repo},
                )
            raise ValueError(f"kickoff: repo {k.repo!r} not in the configured allowlist")
        if self.d.log is not None:
            self.d.log.info("fix kickoff", extra={"workflow": self.spec.name, "repo": k.repo})
        await self.driver.kickoff(k)

    def _repo_allowed(self, repo: str) -> bool:
        """Whether ``repo`` may be targeted by a kickoff. An empty allowlist (REPOS unset)
        imposes no restriction; otherwise the repo must be listed."""
        return not self.d.repos or repo in self.d.repos

    async def resume(self, raw: bytes) -> None:
        """Handle a GitHub check_run webhook. No-op unless the event is this engine's
        check completing — so multiple engines can each be handed the event."""
        ev = parse_check_run_event(raw)
        if ev.check_name != self.spec.check_name or ev.status != "completed":
            return
        await self.driver.resume(
            ResumeInput(
                full_repo=ev.repo_full_name,
                pr_number=ev.pr_number,
                conclusion=ev.conclusion,
                output_text=ev.output_text,
            )
        )

    async def attempt_once(self, rp: RunParams) -> ApplyResult:
        """Run a single fix attempt: triage -> checkout -> analyze -> commit, returning
        the resulting PR. The body the apply_fix tool invokes."""
        if self.d.gh is None:
            raise ValueError("engine: github client is not configured")
        # Triage the (immutable) report into per-file work, re-run on every attempt. A retry
        # resumes on a fresh process (under scale-to-zero, a brand-new Cloud Run instance),
        # so an in-process cache would miss anyway — re-triaging each attempt matches Go/Ko/JS.
        work = await self.spec.triage(self.d.llm, rp.report)  # type: ignore[arg-type]

        cfg = ApplyConfig(
            owner=rp.owner,
            repo=rp.repo,
            clone_url=self._clone_url(rp.owner, rp.repo),
            token=self.d.token,
            ssh_key=self.d.ssh_key,
            base=rp.base,
            branch=self.spec.branch,
            new_branch=rp.new_branch,
            label=self.d.pr_label,
            commit_message=self.spec.commit_message,
            pr_title=self.spec.pr_title,
            pr_body=_pr_body(self.spec, work),
            author=self.d.author,
        )

        git_repo = open_repo(cfg)
        try:
            edits = await self.spec.analyze(
                AnalyzeInput(
                    llm=self.d.llm,  # type: ignore[arg-type]
                    code_llm=self.d.code_llm,
                    repo_dir=git_repo.dir(),
                    work=work,
                    feedback=rp.feedback,
                )
            )
            return commit(self.d.gh, git_repo, cfg, edits)
        finally:
            shutil.rmtree(git_repo.dir(), ignore_errors=True)

    def _clone_url(self, owner: str, repo: str) -> str:
        """Build the clone URL. A test-injected ``clone_url`` wins; otherwise the scheme is
        selected by ``git_transport`` — ``ssh`` builds the ``git@github.com:…`` form, anything
        else the default https URL."""
        if self.d.clone_url is not None:
            return self.d.clone_url(owner, repo)
        if self.d.git_transport == "ssh":
            return f"git@github.com:{owner}/{repo}.git"
        return _default_clone_url(owner, repo)

    def notify(self, title: str, text: str, link: str) -> None:
        if self.d.notify is None:
            return
        try:
            self.d.notify.notify(Message(title=title, text=text, link=link))
        except Exception as exc:  # noqa: BLE001
            # Notifications are best-effort: a transient Slack/Teams failure must not unwind
            # an already-resolved run's terminal path (success/exhausted/timeout), which would
            # lose the message entirely. Log and move on.
            if self.d.log is not None:
                self.d.log.warning("notify failed: title=%s err=%s", title, exc)


# Imported after Engine so RunParams type hints resolve; the driver owns the type.
from automation_agent.agent.fixflow.driver import RunParams  # noqa: E402


def new_engine(spec: Spec, d: Deps) -> Engine:
    """Build an engine, applying defaults."""
    if d.max_iter <= 0:
        d.max_iter = 3
    if d.ci_timeout <= timedelta(0):
        d.ci_timeout = timedelta(minutes=90)
    if d.author.name == "":
        d.author = Author(
            name="automation-agent",
            email="automation-agent@users.noreply.github.com",
        )
    if d.code_llm is None:
        d.code_llm = d.llm
    return Engine(spec, d)


def pull_url(full_repo: str, number: int) -> str:
    return f"https://github.com/{full_repo}/pull/{number}"


def _pr_body(spec: Spec, work: list[FileWork]) -> str:
    lines = [f"Automated {spec.name} fix by automation-agent.\n", "Files addressed:"]
    for f in work:
        lines.append(f"- `{f.path}` ({len(f.items)} item(s))")
    return "\n".join(lines) + "\n"
