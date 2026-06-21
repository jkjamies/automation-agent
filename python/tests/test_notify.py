"""Port of internal/notify/notify_test.go using respx to capture posted bodies."""

from __future__ import annotations

import json

import httpx
import pytest
import respx

from automation_agent.notify import (
    Message,
    SlackNotifier,
    TeamsNotifier,
    new_notifier,
    teams_card,
)

URL = "https://hook.example/webhook"


@respx.mock
def test_slack_notify() -> None:
    route = respx.post(URL).mock(return_value=httpx.Response(200))
    SlackNotifier(URL).notify(
        Message(title="Digest", text="3 commits", link="https://x/pr/1")
    )

    assert route.called
    payload = json.loads(route.calls.last.request.content)
    want = "*Digest*\n3 commits\n<https://x/pr/1>"
    assert payload["text"] == want


@respx.mock
def test_teams_notify() -> None:
    route = respx.post(URL).mock(return_value=httpx.Response(200))
    TeamsNotifier(URL).notify(
        Message(title="Result", text="fixed", link="https://x/pr/2")
    )

    assert route.called
    payload = json.loads(route.calls.last.request.content)
    assert payload["type"] == "message"
    atts = payload["attachments"]
    assert isinstance(atts, list) and len(atts) == 1
    att = atts[0]
    assert att["contentType"] == "application/vnd.microsoft.card.adaptive"
    content = att["content"]
    assert content["type"] == "AdaptiveCard"
    assert "actions" in content


@respx.mock
def test_non_2xx_is_error() -> None:
    respx.post(URL).mock(return_value=httpx.Response(500, text="boom"))
    with pytest.raises(RuntimeError):
        SlackNotifier(URL).notify(Message(text="x"))


def test_new_factory() -> None:
    assert isinstance(new_notifier("slack", "https://hook", ""), SlackNotifier)
    assert isinstance(new_notifier("teams", "", "https://hook"), TeamsNotifier)
    with pytest.raises(ValueError):
        new_notifier("slack", "", "")
    with pytest.raises(ValueError):
        new_notifier("discord", "a", "b")


def test_teams_card_omits_actions_without_link() -> None:
    card = teams_card(Message(title="t", text="b"))
    content = card["attachments"][0]["content"]
    assert "actions" not in content
