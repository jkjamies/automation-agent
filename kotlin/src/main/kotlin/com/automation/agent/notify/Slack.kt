package com.automation.agent.notify

import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import java.net.http.HttpClient

/**
 * Posts to a Slack incoming webhook. The minimal accepted payload is {"text": "..."}.
 */
internal class SlackNotifier(
    private val url: String,
    private val client: HttpClient = defaultClient(),
) : Notifier {
    override suspend fun notify(message: Message) {
        postJson(client, url, buildJsonObject { put("text", slackText(message)) })
    }
}

/** Builds a Slack notifier for the given incoming-webhook URL. */
fun newSlack(url: String): Notifier = SlackNotifier(url)

/** Renders a Message as Slack mrkdwn. */
internal fun slackText(m: Message): String {
    val b = StringBuilder()
    if (m.title.isNotEmpty()) {
        b.append("*").append(m.title).append("*")
    }
    if (m.text.isNotEmpty()) {
        if (b.isNotEmpty()) b.append("\n")
        b.append(m.text)
    }
    if (m.link.isNotEmpty()) {
        if (b.isNotEmpty()) b.append("\n")
        b.append("<").append(m.link).append(">")
    }
    return b.toString()
}
