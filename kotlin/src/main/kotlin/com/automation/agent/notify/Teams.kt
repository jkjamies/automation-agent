package com.automation.agent.notify

import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.addJsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray
import java.net.http.HttpClient

/**
 * Posts an Adaptive Card to a Microsoft Teams incoming webhook. We target the newer
 * Workflows (Power Automate) format rather than the deprecated Office 365 connector
 * MessageCard.
 */
internal class TeamsNotifier(
    private val url: String,
    private val client: HttpClient = defaultClient(),
) : Notifier {
    override suspend fun notify(message: Message) {
        postJson(client, url, teamsCard(message))
    }
}

/** Builds a Teams notifier for the given Workflows webhook URL. */
fun newTeams(url: String): Notifier = TeamsNotifier(url)

/** Builds the Workflows Adaptive Card envelope for a Message. */
internal fun teamsCard(m: Message): JsonObject {
    val content = buildJsonObject {
        put("\$schema", "http://adaptivecards.io/schemas/adaptive-card.json")
        put("type", "AdaptiveCard")
        put("version", "1.2")
        putJsonArray("body") {
            if (m.title.isNotEmpty()) {
                addJsonObject {
                    put("type", "TextBlock")
                    put("text", m.title)
                    put("weight", "Bolder")
                    put("size", "Medium")
                    put("wrap", true)
                }
            }
            if (m.text.isNotEmpty()) {
                addJsonObject {
                    put("type", "TextBlock")
                    put("text", m.text)
                    put("wrap", true)
                }
            }
        }
        if (m.link.isNotEmpty()) {
            putJsonArray("actions") {
                addJsonObject {
                    put("type", "Action.OpenUrl")
                    put("title", "Open")
                    put("url", m.link)
                }
            }
        }
    }

    return buildJsonObject {
        put("type", "message")
        putJsonArray("attachments") {
            addJsonObject {
                put("contentType", "application/vnd.microsoft.card.adaptive")
                put("content", content)
            }
        }
    }
}
