"""The summary workflow's code agents and formatting helpers.

Port of ``internal/agent/summary/summary.go``. The fetch agents write per-repo commit
data to ``commits:<owner/repo>`` state keys; the summarizer's instruction provider reads
them and appends them to the prompt; the notifier posts the ``digest`` state key.
"""

from __future__ import annotations

from collections.abc import AsyncGenerator, Callable
from datetime import UTC, datetime, timedelta
from typing import Protocol, runtime_checkable

from google.adk.agents import BaseAgent
from google.adk.agents.invocation_context import InvocationContext
from google.adk.agents.readonly_context import ReadonlyContext
from google.adk.events import Event
from typing_extensions import override

from automation_agent.agent import setup
from automation_agent.githubapi import Commit
from automation_agent.notify import Message, Notifier

STATE_PREFIX = "commits:"  # one key per repo: commits:<owner/repo>
DIGEST_KEY = "digest"  # summarizer output


@runtime_checkable
class CommitLister(Protocol):
    """The slice of ``githubapi`` the fetchers need (consumer-defined for fakeability).

    ``githubapi.Client`` satisfies this protocol.
    """

    def list_commits_since(
        self, owner: str, repo: str, since: datetime
    ) -> list[Commit]:
        """Return commits to owner/repo authored since the given time."""
        ...


def default_now() -> datetime:
    """The default injectable clock for the summary workflow."""
    return datetime.now(UTC)


class _FetchAgent(BaseAgent):
    """A BaseAgent that fetches the last ``window`` of commits for one repo and writes a
    formatted digest to state under ``commits:<repo>``."""

    _repo: str
    _gh: CommitLister
    _window: timedelta
    _now: Callable[[], datetime]
    model_config = {"arbitrary_types_allowed": True}

    def __init__(
        self,
        repo: str,
        gh: CommitLister,
        window: timedelta,
        now: Callable[[], datetime],
    ) -> None:
        super().__init__(
            name="fetch_" + safe_name(repo),
            description=f"Fetches recent commits for {repo}",
        )
        object.__setattr__(self, "_repo", repo)
        object.__setattr__(self, "_gh", gh)
        object.__setattr__(self, "_window", window)
        object.__setattr__(self, "_now", now)

    @override
    async def _run_async_impl(
        self, ctx: InvocationContext
    ) -> AsyncGenerator[Event, None]:
        owner_name = split_repo(self._repo)
        if owner_name is None:
            raise ValueError(f"invalid repo {self._repo!r} (want owner/repo)")
        owner, name = owner_name
        try:
            commits = self._gh.list_commits_since(
                owner, name, self._now() - self._window
            )
        except Exception as exc:  # noqa: BLE001
            raise RuntimeError(f"fetch {self._repo}: {exc}") from exc
        text = format_commits(self._repo, commits)
        yield setup.text_event(name, text, {STATE_PREFIX + self._repo: text})


class _NotifyAgent(BaseAgent):
    """A BaseAgent that posts the summarizer's digest to chat."""

    _notify: Notifier
    model_config = {"arbitrary_types_allowed": True}

    def __init__(self, notify: Notifier) -> None:
        super().__init__(
            name="notify",
            description="Posts the commit digest to Slack or Teams",
        )
        object.__setattr__(self, "_notify", notify)

    @override
    async def _run_async_impl(
        self, ctx: InvocationContext
    ) -> AsyncGenerator[Event, None]:
        digest = setup.state_string(ctx.session.state, DIGEST_KEY).strip()
        if digest == "":
            digest = "(no digest was produced)"
        try:
            self._notify.notify(Message(title="Daily commit digest", text=digest))
        except Exception as exc:  # noqa: BLE001
            raise RuntimeError(f"notify: {exc}") from exc
        yield setup.text_event("notify", "Posted digest to chat.")


def new_fetch_agent(
    repo: str, gh: CommitLister, window: timedelta, now: Callable[[], datetime]
) -> BaseAgent:
    """Return a code agent that fetches recent commits for ``repo`` into state."""
    return _FetchAgent(repo, gh, window, now)


def new_notify_agent(notify: Notifier) -> BaseAgent:
    """Return a code agent that posts the digest to chat."""
    return _NotifyAgent(notify)


def summary_instruction(
    prompt_body: str,
) -> Callable[[ReadonlyContext], str]:
    """The dynamic instruction for the summarizer: reads the per-repo commit data the
    fetchers wrote to state and appends it to the prompt body.

    Because this returns a callable (an ADK ``InstructionProvider``), ADK bypasses
    ``{key}`` state-injection templating on the result, so commit text containing braces
    is passed through verbatim.
    """

    def provider(ctx: ReadonlyContext) -> str:
        return build_instruction(prompt_body, ctx.state)

    return provider


def build_instruction(prompt_body: str, state: object) -> str:
    """Append every ``commits:*`` string value in ``state`` (sorted by key) to the prompt
    body under a ``## Commits`` heading."""
    items: list[tuple[str, str]] = []
    try:
        pairs = state.items()  # type: ignore[attr-defined]
    except AttributeError:
        pairs = []
    for k, v in pairs:
        if isinstance(k, str) and k.startswith(STATE_PREFIX) and isinstance(v, str):
            items.append((k, v))
    items.sort(key=lambda it: it[0])

    parts = [prompt_body, "\n\n## Commits\n"]
    if not items:
        parts.append("(no commit data)\n")
    for _, value in items:
        parts.append(value)
        parts.append("\n")
    return "".join(parts)


def format_commits(repo: str, commits: list[Commit]) -> str:
    """Format a repo's commits exactly like Go's ``formatCommits``."""
    if not commits:
        return f"Repository {repo}: no commits in the window."
    lines = [f"Repository {repo} ({len(commits)} commits):\n"]
    for c in commits:
        lines.append(
            f"- {short_sha(c.sha)} {first_line(c.message)} ({c.author})\n"
        )
    return "".join(lines)


def first_line(s: str) -> str:
    """Return the first line of ``s``, trimmed."""
    i = s.find("\n")
    if i >= 0:
        return s[:i].strip()
    return s.strip()


def short_sha(sha: str) -> str:
    """Return the 7-character short SHA (or the whole SHA if shorter)."""
    if len(sha) > 7:
        return sha[:7]
    return sha


def split_repo(s: str) -> tuple[str, str] | None:
    """Split ``owner/repo`` into its parts, or None if malformed."""
    owner, sep, repo = s.partition("/")
    if not sep or owner == "" or repo == "":
        return None
    return owner, repo


def safe_name(s: str) -> str:
    """Map ``s`` to an agent-name-safe string (non-alphanumerics become ``_``)."""
    out = []
    for ch in s:
        if ch.isascii() and ch.isalnum():
            out.append(ch)
        else:
            out.append("_")
    return "".join(out)
