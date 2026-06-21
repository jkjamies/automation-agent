"""Microsoft Teams notifier.

Posts an Adaptive Card to a Teams incoming webhook. We target the newer
Workflows (Power Automate) format rather than the deprecated Office 365
connector MessageCard.
"""

from __future__ import annotations

from typing import Any

from automation_agent.notify.notify import Message, post_json


class TeamsNotifier:
    """Posts an Adaptive Card to a Microsoft Teams incoming webhook."""

    def __init__(self, url: str) -> None:
        self.url = url

    def notify(self, m: Message) -> None:
        """Post the message as a Workflows Adaptive Card."""
        post_json(self.url, teams_card(m))


def teams_card(m: Message) -> dict[str, Any]:
    """Build the Workflows Adaptive Card envelope for a Message."""
    body: list[dict[str, Any]] = []
    if m.title != "":
        body.append(
            {
                "type": "TextBlock",
                "text": m.title,
                "weight": "Bolder",
                "size": "Medium",
                "wrap": True,
            }
        )
    if m.text != "":
        body.append({"type": "TextBlock", "text": m.text, "wrap": True})

    content: dict[str, Any] = {
        "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
        "type": "AdaptiveCard",
        "version": "1.2",
        "body": body,
    }
    if m.link != "":
        content["actions"] = [
            {"type": "Action.OpenUrl", "title": "Open", "url": m.link},
        ]

    return {
        "type": "message",
        "attachments": [
            {
                "contentType": "application/vnd.microsoft.card.adaptive",
                "content": content,
            },
        ],
    }
