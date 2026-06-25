package com.automation.agent.notify

import com.sun.net.httpserver.HttpServer
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import java.io.IOException
import java.net.InetSocketAddress

private class Capture {
    @Volatile
    var body: ByteArray = ByteArray(0)
}

/** Starts a local server that records the body of the next POST and replies with [status]. */
private fun captureServer(status: Int): Pair<HttpServer, Capture> {
    val cap = Capture()
    val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
    server.createContext("/") { exchange ->
        cap.body = exchange.requestBody.readBytes()
        exchange.sendResponseHeaders(status, -1)
        exchange.close()
    }
    server.start()
    return server to cap
}

private fun HttpServer.url(): String = "http://127.0.0.1:${address.port}/"

class NotifyTest : BehaviorSpec({
    Given("a Slack notifier and a capturing webhook") {
        When("notifying a message with title, text and link") {
            Then("the payload is rendered as Slack mrkdwn") {
                val (srv, cap) = captureServer(200)
                try {
                    newSlack(srv.url())
                        .notify(Message(title = "Digest", text = "3 commits", link = "https://x/pr/1"))
                    val payload = Json.parseToJsonElement(String(cap.body)).jsonObject
                    payload.getValue("text").jsonPrimitive.content shouldBe "*Digest*\n3 commits\n<https://x/pr/1>"
                } finally {
                    srv.stop(0)
                }
            }
        }
    }

    Given("a Teams notifier and a capturing webhook") {
        When("notifying a message with a link") {
            Then("it posts a Workflows adaptive card with an action") {
                val (srv, cap) = captureServer(200)
                try {
                    newTeams(srv.url())
                        .notify(Message(title = "Result", text = "fixed", link = "https://x/pr/2"))
                    val payload = Json.parseToJsonElement(String(cap.body)).jsonObject
                    payload.getValue("type").jsonPrimitive.content shouldBe "message"
                    val atts = payload.getValue("attachments").jsonArray
                    atts.size shouldBe 1
                    val att = atts[0].jsonObject
                    att.getValue("contentType").jsonPrimitive.content shouldBe
                        "application/vnd.microsoft.card.adaptive"
                    val content = att.getValue("content").jsonObject
                    content.getValue("type").jsonPrimitive.content shouldBe "AdaptiveCard"
                    content.containsKey("actions") shouldBe true
                } finally {
                    srv.stop(0)
                }
            }
        }
    }

    Given("a webhook that returns a 500 status") {
        When("notifying via Slack") {
            Then("it raises an IOException") {
                val (srv, _) = captureServer(500)
                try {
                    shouldThrow<IOException> {
                        newSlack(srv.url()).notify(Message(text = "x"))
                    }
                } finally {
                    srv.stop(0)
                }
            }
        }
    }

    Given("the notifier factory") {
        When("given valid providers and matching urls") {
            Then("it builds a notifier for each") {
                newNotifier("slack", "https://hook", "")
                newNotifier("teams", "", "https://hook")
            }
        }
        When("given a missing url or an unknown provider") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> { newNotifier("slack", "", "") }
                shouldThrow<IllegalArgumentException> { newNotifier("discord", "a", "b") }
            }
        }
    }

    Given("a Teams message without a link") {
        When("building the adaptive card") {
            Then("it omits the actions block") {
                val card = teamsCard(Message(title = "t", text = "b"))
                val content = card.getValue("attachments").jsonArray[0].jsonObject.getValue("content").jsonObject
                content.containsKey("actions") shouldBe false
            }
        }
    }
})
