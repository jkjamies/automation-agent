# automation_agent/notify

Posts provider-agnostic `Message`s to Slack or Microsoft Teams behind one
`Notifier` protocol, so the choice is a config flag (`NOTIFY_PROVIDER`).

## Flow

```mermaid
flowchart TD
    Caller[workflow] --> NEW["new(provider, slack_url, teams_url)"]
    NEW --> SW{provider switch}
    SW -->|"slack & slack_url set"| NS["new_slack(url)"]
    SW -->|"slack & url empty"| ES[error: SLACK_WEBHOOK_URL required]
    SW -->|"teams & teams_url set"| NT["new_teams(url)"]
    SW -->|"teams & url empty"| ET[error: TEAMS_WEBHOOK_URL required]
    SW -->|other| EU[error: unknown provider]

    NS --> SN[SlackNotifier]
    NT --> TN[TeamsNotifier]
    SN -.implements.-> IF["Notifier.notify(Message)"]
    TN -.implements.-> IF

    Caller -->|"Message{title, text, link}"| IF
    SN -->|"notify"| SR["slack_text(m) -> {'text': mrkdwn}"]
    TN -->|"notify"| TR["teams_card(m) -> Adaptive Card (Workflows)"]
    SR --> PJ["post_json(httpc, url, payload)"]
    TR --> PJ
    PJ -->|"json + POST application/json"| HTTP[(HTTP POST -> Slack/Teams webhook)]
    HTTP --> RESP{2xx?}
    RESP -->|yes| OK[None]
    RESP -->|non-2xx| ERR[error: notification rejected + body snippet]
```

- `slack.py` — Slack incoming webhook (`{"text": ...}`, mrkdwn).
- `teams.py` — Teams **Workflows / Adaptive Card** format (the O365 connector
  MessageCard path is deprecated; we target the new one).
- `new(provider, slack_url, teams_url)` picks the implementation.

Deterministic tooling — no agent imports. Tested with `respx` capturing the
posted body; no real Slack/Teams calls.
