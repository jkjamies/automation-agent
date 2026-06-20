# internal/notify

Posts provider-agnostic `Message`s to Slack or Microsoft Teams behind one
`Notifier` interface, so the choice is a config flag (`NOTIFY_PROVIDER`).

- `slack.go` — Slack incoming webhook (`{"text": ...}`, mrkdwn).
- `teams.go` — Teams **Workflows / Adaptive Card** format (the O365 connector
  MessageCard path is deprecated; we target the new one).
- `New(provider, slackURL, teamsURL)` picks the implementation.

Deterministic tooling — no agent imports. Tested with `httptest` capturing the
posted body; no real Slack/Teams calls.
