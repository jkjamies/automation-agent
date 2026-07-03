package com.automation.agent.githubapi

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldContain
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respond
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.http.content.TextContent
import io.ktor.http.headersOf
import io.ktor.serialization.kotlinx.json.json
import kotlinx.serialization.json.Json

/** A captured request: method, path, and (text) body. */
private data class Seen(val method: String, val path: String, val body: String)

/** Builds a client whose mock records every request and answers per-route with a status + body. */
private fun recordingClient(
    seen: MutableList<Seen>,
    routes: Map<String, Pair<HttpStatusCode, String>>,
    baseUrl: String = "https://api.github.test/",
    authoredLogin: String = "",
    appAuthored: Boolean = false,
): Client {
    val engine = MockEngine { request ->
        val body = when (val b = request.body) {
            is TextContent -> b.text
            else -> ""
        }
        seen += Seen(request.method.value, request.url.encodedPath, body)
        val route = routes["${request.method.value} ${request.url.encodedPath}"]
        if (route == null) {
            respond("", HttpStatusCode.NotFound)
        } else {
            respond(route.second, route.first, headersOf(HttpHeaders.ContentType, "application/json"))
        }
    }
    val http = HttpClient(engine) { install(ContentNegotiation) { json(Json { ignoreUnknownKeys = true }) } }
    return Client(baseUrl = baseUrl, httpClient = http, authoredLogin = authoredLogin, appAuthored = appAuthored)
}

class GitHubApiReviewerTest : BehaviorSpec({
    Given("createReview") {
        Then("it posts a COMMENT review with the inline comments") {
            val seen = mutableListOf<Seen>()
            val c = recordingClient(seen, mapOf("POST /repos/o/r/pulls/5/reviews" to (HttpStatusCode.OK to "{}")))
            c.createReview("o", "r", 5, ReviewInput(body = "hi", comments = listOf(ReviewComment("a.kt", 11, "RIGHT", "b"))))
            val req = seen.single()
            req.method shouldBe "POST"
            req.body shouldContain "\"event\":\"COMMENT\""
            req.body shouldContain "\"path\":\"a.kt\""
            req.body shouldContain "\"side\":\"RIGHT\""
        }
    }

    Given("createCheckRun") {
        Then("it posts a completed advisory check") {
            val seen = mutableListOf<Seen>()
            val c = recordingClient(seen, mapOf("POST /repos/o/r/check-runs" to (HttpStatusCode.OK to "{}")))
            c.createCheckRun("o", "r", CheckRunInput(name = "agent-review", headSha = "sha", conclusion = "neutral", title = "t", summary = "s"))
            seen.single().body shouldContain "\"status\":\"completed\""
            seen.single().body shouldContain "\"conclusion\":\"neutral\""
        }
        Then("it rejects a non-advisory conclusion at the boundary (no request)") {
            val seen = mutableListOf<Seen>()
            val c = recordingClient(seen, emptyMap())
            shouldThrow<IllegalArgumentException> {
                c.createCheckRun("o", "r", CheckRunInput(name = "agent-review", headSha = "sha", conclusion = "failure"))
            }
            seen.isEmpty() shouldBe true
        }
    }

    Given("minimizeComment") {
        Then("it POSTs the OUTDATED mutation to the derived GraphQL endpoint") {
            val seen = mutableListOf<Seen>()
            val c = recordingClient(seen, mapOf("POST /graphql" to (HttpStatusCode.OK to "{\"data\":{}}")))
            c.minimizeComment("node-1")
            val req = seen.single()
            req.path shouldBe "/graphql"
            req.body shouldContain "minimizeComment"
            req.body shouldContain "OUTDATED"
        }
        Then("a GraphQL errors array becomes a failure") {
            val seen = mutableListOf<Seen>()
            val c = recordingClient(seen, mapOf("POST /graphql" to (HttpStatusCode.OK to "{\"errors\":[{\"message\":\"nope\"}]}")))
            shouldThrow<Exception> { c.minimizeComment("node-1") }
        }
        Then("a GitHub Enterprise base maps /api/v3 to /api/graphql") {
            val seen = mutableListOf<Seen>()
            val c = recordingClient(
                seen,
                mapOf("POST /api/graphql" to (HttpStatusCode.OK to "{\"data\":{}}")),
                baseUrl = "https://ghe.test/api/v3/",
            )
            c.minimizeComment("node-1")
            seen.map { it.path } shouldContain "/api/graphql"
        }
    }

    Given("upsertMarkerComment") {
        val marker = "<!-- automation-agent:review:o/r#5 -->"
        Then("it rejects an empty marker or a body missing the marker") {
            val c = recordingClient(mutableListOf(), emptyMap())
            shouldThrow<IllegalArgumentException> { c.upsertMarkerComment("o", "r", 5, "", "body") }
            shouldThrow<IllegalArgumentException> { c.upsertMarkerComment("o", "r", 5, marker, "no marker here") }
        }
        Then("with a known login it edits its own existing comment in place") {
            val seen = mutableListOf<Seen>()
            val list = "[{\"id\":42,\"body\":\"old $marker\",\"user\":{\"login\":\"me\",\"type\":\"Bot\"}}]"
            val c = recordingClient(
                seen,
                mapOf(
                    "GET /repos/o/r/issues/5/comments" to (HttpStatusCode.OK to list),
                    "PATCH /repos/o/r/issues/comments/42" to (HttpStatusCode.OK to "{}"),
                ),
                authoredLogin = "me",
            )
            c.upsertMarkerComment("o", "r", 5, marker, "new $marker")
            seen.map { "${it.method} ${it.path}" } shouldContain "PATCH /repos/o/r/issues/comments/42"
        }
        Then("with no existing owned comment it creates one") {
            val seen = mutableListOf<Seen>()
            val c = recordingClient(
                seen,
                mapOf(
                    "GET /repos/o/r/issues/5/comments" to (HttpStatusCode.OK to "[]"),
                    "POST /repos/o/r/issues/5/comments" to (HttpStatusCode.OK to "{}"),
                ),
                authoredLogin = "me",
            )
            c.upsertMarkerComment("o", "r", 5, marker, "new $marker")
            seen.map { "${it.method} ${it.path}" } shouldContain "POST /repos/o/r/issues/5/comments"
        }
        Then("on the author-type fallback a 403 edit is skipped and a fresh comment is created") {
            val seen = mutableListOf<Seen>()
            // authoredLogin unresolved + appAuthored -> trusts a Bot comment, but the edit 403s
            // (foreign bot echoing the marker), so it must fall through to create.
            val list = "[{\"id\":42,\"body\":\"old $marker\",\"user\":{\"login\":\"other\",\"type\":\"Bot\"}}]"
            val c = recordingClient(
                seen,
                mapOf(
                    "GET /repos/o/r/issues/5/comments" to (HttpStatusCode.OK to list),
                    "PATCH /repos/o/r/issues/comments/42" to (HttpStatusCode.Forbidden to "{}"),
                    "POST /repos/o/r/issues/5/comments" to (HttpStatusCode.OK to "{}"),
                ),
                authoredLogin = "",
                appAuthored = true,
            )
            c.upsertMarkerComment("o", "r", 5, marker, "new $marker")
            seen.map { "${it.method} ${it.path}" } shouldContain "POST /repos/o/r/issues/5/comments"
        }
    }
})
