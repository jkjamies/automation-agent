"""notify — post provider-agnostic messages to Slack or Teams."""

from automation_agent.notify.notify import (
    Message,
    Notifier,
    new_notifier,
    post_json,
)
from automation_agent.notify.slack import SlackNotifier, slack_text
from automation_agent.notify.teams import TeamsNotifier, teams_card

__all__ = [
    "Message",
    "Notifier",
    "SlackNotifier",
    "TeamsNotifier",
    "new_notifier",
    "post_json",
    "slack_text",
    "teams_card",
]
