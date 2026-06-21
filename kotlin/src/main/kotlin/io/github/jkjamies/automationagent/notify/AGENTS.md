# notify

Posts provider-agnostic `Message`s to Slack or Microsoft Teams behind one `Notifier`
interface, so the choice is a config flag (`NOTIFY_PROVIDER`). Deterministic tooling —
**no agent imports**.

## Details

- `Notify.kt` — `Message`, `Notifier` (a `suspend` interface; cancellation rides on the
  coroutine context), `newNotifier(provider, slackUrl, teamsUrl)` factory (throws on an
  unknown provider or missing URL), and `postJson` (uses `java.net.http.HttpClient` off
  `Dispatchers.IO`; non-2xx → `IOException` with a body snippet).
- `Slack.kt` — Slack incoming webhook (`{"text": ...}`, mrkdwn via `slackText`).
- `Teams.kt` — Teams **Workflows / Adaptive Card** format via `teamsCard` (the O365
  connector MessageCard path is deprecated; we target the new one).
- JSON is built with `kotlinx.serialization` (`buildJsonObject`).

Tested with the JDK's `com.sun.net.httpserver.HttpServer` capturing the posted body; no
real Slack/Teams calls.
