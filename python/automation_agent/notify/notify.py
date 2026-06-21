"""Post provider-agnostic messages to a chat destination (Slack or Teams).

Both providers sit behind a single :class:`Notifier` protocol, so the workflow
choice is a config flag, not a code change. Deterministic tooling — it must not
import agents.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Protocol, runtime_checkable

import httpx


@dataclass
class Message:
    """A provider-agnostic notification."""

    title: str = ""  # short bold heading
    text: str = ""  # body
    link: str = ""  # optional URL (e.g. a PR) rendered as an action/link


@runtime_checkable
class Notifier(Protocol):
    """Posts messages to a chat destination."""

    def notify(self, m: Message) -> None:
        """Post the message, raising on failure."""
        ...


def new_notifier(provider: str, slack_url: str, teams_url: str) -> Notifier:
    """Return a Notifier for the given provider ("slack" or "teams").

    Raises if the required webhook URL is empty or the provider is unknown.
    """
    if provider == "slack":
        if slack_url == "":
            raise ValueError(
                "SLACK_WEBHOOK_URL is required for notify provider slack"
            )
        from automation_agent.notify.slack import SlackNotifier

        return SlackNotifier(slack_url)
    if provider == "teams":
        if teams_url == "":
            raise ValueError(
                "TEAMS_WEBHOOK_URL is required for notify provider teams"
            )
        from automation_agent.notify.teams import TeamsNotifier

        return TeamsNotifier(teams_url)
    raise ValueError(f"unknown notify provider {provider!r} (want slack|teams)")


def post_json(url: str, payload: Any) -> None:
    """POST ``payload`` as JSON, raising on a non-2xx status."""
    with httpx.Client(timeout=10.0) as client:
        resp = client.post(
            url, json=payload, headers={"Content-Type": "application/json"}
        )
    if resp.status_code < 200 or resp.status_code >= 300:
        snippet = resp.text[:512]
        raise RuntimeError(
            f"notification rejected: {resp.status_code} {resp.reason_phrase}: {snippet}"
        )
