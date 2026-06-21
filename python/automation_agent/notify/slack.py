"""Slack incoming-webhook notifier.

The minimal accepted payload is ``{"text": "..."}``.
"""

from __future__ import annotations

from automation_agent.notify.notify import Message, post_json


class SlackNotifier:
    """Posts to a Slack incoming webhook."""

    def __init__(self, url: str) -> None:
        self.url = url

    def notify(self, m: Message) -> None:
        """Post the message as Slack mrkdwn."""
        post_json(self.url, {"text": slack_text(m)})


def slack_text(m: Message) -> str:
    """Render a Message as Slack mrkdwn."""
    parts: list[str] = []
    if m.title != "":
        parts.append(f"*{m.title}*")
    if m.text != "":
        parts.append(m.text)
    if m.link != "":
        parts.append(f"<{m.link}>")
    return "\n".join(parts)
