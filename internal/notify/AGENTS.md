# internal/notify

Posts provider-agnostic `Message`s to Slack or Microsoft Teams behind one
`Notifier` interface, so the choice is a config flag (`NOTIFY_PROVIDER`).

## Flow

```mermaid
flowchart TD
    Caller[workflow] --> NEW["New(provider, slackURL, teamsURL)"]
    NEW --> SW{provider switch}
    SW -->|"slack & slackURL set"| NS["NewSlack(url)"]
    SW -->|"slack & url empty"| ES[error: SLACK_WEBHOOK_URL required]
    SW -->|"teams & teamsURL set"| NT["NewTeams(url)"]
    SW -->|"teams & url empty"| ET[error: TEAMS_WEBHOOK_URL required]
    SW -->|other| EU[error: unknown provider]

    NS --> SN[slackNotifier]
    NT --> TN[teamsNotifier]
    SN -.implements.-> IF["Notifier.Notify(ctx, Message)"]
    TN -.implements.-> IF

    Caller -->|"Message{Title, Text, Link}"| IF
    SN -->|"Notify"| SR["slackText(m) -> {\"text\": mrkdwn}"]
    TN -->|"Notify"| TR["teamsCard(m) -> Adaptive Card (Workflows)"]
    SR --> PJ["postJSON(ctx, httpc, url, payload)"]
    TR --> PJ
    PJ -->|"json.Marshal + POST application/json"| HTTP[(HTTP POST -> Slack/Teams webhook)]
    HTTP --> RESP{2xx?}
    RESP -->|yes| OK[nil]
    RESP -->|non-2xx| ERR[error: notification rejected + body snippet]
```

- `slack.go` — Slack incoming webhook (`{"text": ...}`, mrkdwn).
- `teams.go` — Teams **Workflows / Adaptive Card** format (the O365 connector
  MessageCard path is deprecated; we target the new one).
- `New(provider, slackURL, teamsURL)` picks the implementation.

Deterministic tooling — no agent imports. Tested with `httptest` capturing the
posted body; no real Slack/Teams calls.
