/*
 * Package notify posts provider-agnostic messages to a chat destination (Slack or Microsoft
 * Teams) behind a single interface, so the workflow choice is a config flag, not a code
 * change. It is deterministic tooling — it must not import agents.
 */
package io.github.jkjamies.automationagent.notify

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import java.io.IOException
import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.time.Duration

/** Message is a provider-agnostic notification. */
data class Message(
    val title: String = "", // short bold heading
    val text: String = "", // body
    val link: String = "", // optional URL (e.g. a PR) rendered as an action/link
)

/** Notifier posts messages to a chat destination. */
interface Notifier {
    suspend fun notify(message: Message)
}

/**
 * Returns a Notifier for the given provider ("slack" or "teams") using the matching webhook
 * URL. Throws [IllegalArgumentException] for an unknown provider or a missing URL.
 */
fun newNotifier(provider: String, slackUrl: String, teamsUrl: String): Notifier =
    when (provider) {
        "slack" -> {
            require(slackUrl.isNotEmpty()) { "SLACK_WEBHOOK_URL is required for notify provider slack" }
            newSlack(slackUrl)
        }
        "teams" -> {
            require(teamsUrl.isNotEmpty()) { "TEAMS_WEBHOOK_URL is required for notify provider teams" }
            newTeams(teamsUrl)
        }
        else -> throw IllegalArgumentException("unknown notify provider \"$provider\" (want slack|teams)")
    }

/** The HTTP client used by notifiers unless one is injected. */
internal fun defaultClient(): HttpClient =
    HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(10)).build()

internal val notifyJson: Json = Json { encodeDefaults = true }

/** Serializes [payload] and POSTs it, throwing an [IOException] on a non-2xx status. */
internal suspend fun postJson(client: HttpClient, url: String, payload: JsonObject) {
    val body = notifyJson.encodeToString(JsonObject.serializer(), payload)
    val request = HttpRequest.newBuilder(URI.create(url))
        .header("Content-Type", "application/json")
        .timeout(Duration.ofSeconds(10))
        .POST(HttpRequest.BodyPublishers.ofString(body))
        .build()

    val response = withContext(Dispatchers.IO) {
        client.send(request, HttpResponse.BodyHandlers.ofString())
    }
    val status = response.statusCode()
    if (status < 200 || status >= 300) {
        val snippet = (response.body() ?: "").take(512)
        throw IOException("notification rejected: $status: $snippet")
    }
}
