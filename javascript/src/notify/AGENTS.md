# src/notify

Posts provider-agnostic `Message`s to Slack or Microsoft Teams behind one
`Notifier` interface, so the choice is a config flag (`NOTIFY_PROVIDER`).

```mermaid
flowchart TD
    Caller[workflow] --> NEW["newNotifier(provider, slackUrl, teamsUrl)"]
    NEW --> SW{provider switch}
    SW -->|"slack & url set"| NS[SlackNotifier]
    SW -->|"slack & url empty"| ES[throw: SLACK_WEBHOOK_URL required]
    SW -->|"teams & url set"| NT[TeamsNotifier]
    SW -->|"teams & url empty"| ET[throw: TEAMS_WEBHOOK_URL required]
    SW -->|other| EU[throw: unknown provider]

    NS -.implements.-> IF["Notifier.notify(Message)"]
    NT -.implements.-> IF
    Caller -->|"Message{title, text, link}"| IF
    NS -->|"notify"| SR["slackText(m) -> {text: mrkdwn}"]
    NT -->|"notify"| TR["teamsCard(m) -> Adaptive Card (Workflows)"]
    SR --> PJ["postJson(url, payload)"]
    TR --> PJ
    PJ -->|"fetch POST application/json"| HTTP[(Slack/Teams webhook)]
    HTTP --> RESP{2xx?}
    RESP -->|yes| OK[resolve]
    RESP -->|non-2xx| ERR[throw: notification rejected + body snippet]
```

- `slack.ts` — Slack incoming webhook (`{ text: ... }`, mrkdwn).
- `teams.ts` — Teams **Workflows / Adaptive Card** format (the O365 connector
  MessageCard path is deprecated; we target the new one).
- `newNotifier(provider, slackUrl, teamsUrl)` picks the implementation.

Deterministic tooling — no agent imports. Tested by stubbing `fetch` and asserting on
the posted body; no real Slack/Teams calls.
