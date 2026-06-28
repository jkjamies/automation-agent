package com.automation.agent.githubapi

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldContain
import io.kotest.matchers.shouldBe
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respond
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.http.headersOf
import io.ktor.serialization.kotlinx.json.json
import kotlinx.serialization.json.Json
import java.time.Instant
import java.util.Base64

private fun mockClient(
    routes: Map<String, String>,
    hits: MutableSet<String>,
    queries: MutableMap<String, String>,
): HttpClient {
    val engine = MockEngine { request ->
        val key = "${request.method.value} ${request.url.encodedPath}"
        hits += key
        request.url.parameters.names().forEach { name ->
            request.url.parameters[name]?.let { queries[name] = it }
        }
        val body = routes[key]
        if (body == null) {
            respond("", HttpStatusCode.NotFound)
        } else {
            respond(body, HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json"))
        }
    }
    return HttpClient(engine) {
        install(ContentNegotiation) { json(Json { ignoreUnknownKeys = true }) }
    }
}

private fun client(
    routes: Map<String, String>,
    hits: MutableSet<String> = mutableSetOf(),
    queries: MutableMap<String, String> = mutableMapOf(),
): Client =
    Client(baseUrl = "https://api.github.test/", httpClient = mockClient(routes, hits, queries))

class GitHubApiTest : BehaviorSpec({
    Given("a repo with one recent commit") {
        When("listing commits since the epoch") {
            Then("it projects the commit fields including the author date") {
                val c = client(
                    mapOf(
                        "GET /repos/o/r/commits" to
                            """[{"sha":"abc","html_url":"https://gh/abc","commit":{"message":"fix bug","author":{"name":"Jane","date":"2026-06-19T10:00:00Z"}}}]""",
                    ),
                )
                val commits = c.listCommitsSince("o", "r", Instant.EPOCH)
                commits.size shouldBe 1
                commits[0].sha shouldBe "abc"
                commits[0].author shouldBe "Jane"
                commits[0].message shouldBe "fix bug"
                commits[0].url shouldBe "https://gh/abc"
                commits[0].at shouldBe Instant.parse("2026-06-19T10:00:00Z")
            }
        }
    }

    Given("a repo accepting a new PR and labels") {
        When("creating a PR and adding a label") {
            Then("it projects the PR and calls the labels endpoint") {
                val hits = mutableSetOf<String>()
                val c = client(
                    mapOf(
                        "POST /repos/o/r/pulls" to
                            """{"number":5,"title":"fix lint","html_url":"https://gh/pr/5","head":{"ref":"agent/fix","sha":"deadbeef"}}""",
                        "POST /repos/o/r/issues/5/labels" to """[{"name":"automation-agent"}]""",
                    ),
                    hits,
                )
                val pr = c.createPr("o", "r", PrInput(title = "fix lint", head = "agent/fix", base = "main"))
                pr.number shouldBe 5
                pr.branch shouldBe "agent/fix"
                pr.headSha shouldBe "deadbeef"
                pr.url shouldBe "https://gh/pr/5"
                c.addLabels("o", "r", 5, listOf("automation-agent"))
                hits shouldContain "POST /repos/o/r/issues/5/labels"
            }
        }
    }

    Given("an open PR for a branch") {
        When("finding the open PR by branch") {
            Then("it returns the matching PR and filters by state=open + head=owner:branch") {
                val queries = mutableMapOf<String, String>()
                val c = client(
                    mapOf(
                        "GET /repos/o/r/pulls" to
                            """[{"number":5,"head":{"ref":"agent/fix","sha":"s5"},"labels":[{"name":"automation-agent"}]}]""",
                    ),
                    queries = queries,
                )
                c.findOpenPrByBranch("o", "r", "agent/fix")?.number shouldBe 5
                // Assert the branch filter, not just the response mapping: a regression to an
                // unfiltered pulls list must fail here (mirrors the Python client test).
                queries["state"] shouldBe "open"
                queries["head"] shouldBe "o:agent/fix"
            }
        }
        When("no open PR exists for the branch") {
            Then("it returns null") {
                val c = client(mapOf("GET /repos/o/r/pulls" to "[]"))
                c.findOpenPrByBranch("o", "r", "nope") shouldBe null
            }
        }
    }

    Given("a base...head comparison") {
        When("comparing two refs") {
            Then("it projects the commit count and changed files") {
                val c = client(
                    mapOf(
                        "GET /repos/o/r/compare/main...agent-fix" to
                            """{"total_commits":2,"files":[{"filename":"a.kt","status":"modified","additions":3,"deletions":1},{"filename":"b.kt","status":"added","additions":10,"deletions":0}]}""",
                    ),
                )
                val cmp = c.compare("o", "r", "main", "agent-fix")
                cmp.totalCommits shouldBe 2
                cmp.files.size shouldBe 2
                cmp.files[0].path shouldBe "a.kt"
                cmp.files[0].status shouldBe "modified"
                cmp.files[0].additions shouldBe 3
                cmp.files[1].path shouldBe "b.kt"
            }
        }
    }

    Given("a base64-encoded file") {
        When("getting its content") {
            Then("it decodes to the original text") {
                val encoded = Base64.getEncoder().encodeToString("package foo\n".toByteArray())
                val c = client(
                    mapOf(
                        "GET /repos/o/r/contents/internal/foo.go" to
                            """{"type":"file","encoding":"base64","path":"internal/foo.go","content":"$encoded"}""",
                    ),
                )
                c.getFileContent("o", "r", "internal/foo.go", "main") shouldBe "package foo\n"
            }
        }
    }

    Given("a check_run completed webhook body") {
        When("parsing the event") {
            Then("it extracts action, correlation and output") {
                val body = """
                    {
                      "action":"completed",
                      "check_run":{
                        "name":"agent-lint-verify",
                        "status":"completed",
                        "conclusion":"failure",
                        "head_sha":"sha123",
                        "output":{"text":"errcheck: unchecked error"},
                        "pull_requests":[{"number":12,"head":{"ref":"agent/fix"}}]
                      },
                      "repository":{"full_name":"acme/api"}
                    }
                """.trimIndent()
                val ev = Client.parseCheckRunEvent(body)
                ev.action shouldBe "completed"
                ev.checkName shouldBe "agent-lint-verify"
                ev.conclusion shouldBe "failure"
                ev.headSha shouldBe "sha123"
                ev.prNumber shouldBe 12
                ev.prBranch shouldBe "agent/fix"
                ev.repoFullName shouldBe "acme/api"
                ev.outputText shouldBe "errcheck: unchecked error"
            }
        }
    }

    Given("a ref with a completed agent check and a ref without one") {
        When("reading the agent check") {
            Then("present returns the result and absent returns found=false") {
                val queries = mutableMapOf<String, String>()
                val c = client(
                    mapOf(
                        "GET /repos/o/r/commits/sha1/check-runs" to
                            """{"total_count":1,"check_runs":[{"name":"agent-lint-verify","status":"completed","conclusion":"success","completed_at":"2026-06-19T11:00:00Z","output":{"summary":"all checks passed"}}]}""",
                        "GET /repos/o/r/commits/sha2/check-runs" to
                            """{"total_count":0,"check_runs":[]}""",
                    ),
                    queries = queries,
                )
                val res = c.agentCheck("o", "r", "sha1", "agent-lint-verify")
                res.found shouldBe true
                res.status shouldBe "completed"
                res.conclusion shouldBe "success"
                res.outputText shouldBe "all checks passed"
                // Assert the request filters by name AND filter=latest: without filter=latest GitHub
                // returns every historical run per name and we could read a stale prior run (mirrors
                // the Go reference's Filter: ptr("latest")).
                queries["check_name"] shouldBe "agent-lint-verify"
                queries["filter"] shouldBe "latest"

                val missing = c.agentCheck("o", "r", "sha2", "agent-lint-verify")
                missing.found shouldBe false
            }
        }
    }

    Given("a token source") {
        When("the client makes requests") {
            Then("it injects a fresh Bearer token per request") {
                val auths = mutableListOf<String>()
                val engine = MockEngine { request ->
                    auths += request.headers[HttpHeaders.Authorization] ?: ""
                    respond("[]", HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json"))
                }
                var n = 0
                val c = Client(
                    tokenSource = { "tok-${++n}" }, // a fresh (refreshed) token each request
                    baseUrl = "https://api.github.test/",
                    httpClient = HttpClient(engine) {
                        install(ContentNegotiation) { json(Json { ignoreUnknownKeys = true }) }
                    },
                )
                c.listCommitsSince("o", "r", Instant.EPOCH)
                c.listCommitsSince("o", "r", Instant.EPOCH)
                auths shouldBe listOf("Bearer tok-1", "Bearer tok-2")
            }
        }
        When("there is no token source") {
            Then("requests are anonymous") {
                val auths = mutableListOf<String>()
                val engine = MockEngine { request ->
                    auths += request.headers[HttpHeaders.Authorization] ?: ""
                    respond("[]", HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json"))
                }
                val c = Client(
                    baseUrl = "https://api.github.test/",
                    httpClient = HttpClient(engine) {
                        install(ContentNegotiation) { json(Json { ignoreUnknownKeys = true }) }
                    },
                )
                c.listCommitsSince("o", "r", Instant.EPOCH)
                auths shouldBe listOf("")
            }
        }
    }
})
