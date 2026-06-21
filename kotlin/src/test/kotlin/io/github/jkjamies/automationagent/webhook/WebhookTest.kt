package io.github.jkjamies.automationagent.webhook

import io.github.jkjamies.automationagent.ingest.Envelope
import io.github.jkjamies.automationagent.ingest.Kind
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import io.ktor.client.request.header
import io.ktor.client.request.post
import io.ktor.client.request.get
import io.ktor.client.request.setBody
import io.ktor.client.statement.bodyAsText
import io.ktor.http.HttpStatusCode
import io.ktor.server.testing.testApplication
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

private class Capture(private val fail: Boolean = false) {
    var env: Envelope? = null
    val ingest = IngestFunc { e ->
        env = e
        if (fail) throw RuntimeException("boom")
    }
}

private fun sign(secret: String, body: String): String {
    val mac = Mac.getInstance("HmacSHA256")
    mac.init(SecretKeySpec(secret.toByteArray(), "HmacSHA256"))
    return "sha256=" + mac.doFinal(body.toByteArray()).joinToString("") { "%02x".format(it.toInt() and 0xFF) }
}

class WebhookTest : BehaviorSpec({
    Given("a lint kickoff request") {
        When("POSTing to /webhooks/lint") {
            Then("it accepts and emits a LINT envelope with the body") {
                val c = Capture()
                testApplication {
                    application { webhookRoutes(c.ingest) }
                    val resp = client.post("/webhooks/lint") { setBody("{\"problems\":[]}") }
                    resp.status shouldBe HttpStatusCode.Accepted
                }
                val env = c.env.shouldNotBeNull()
                env.kind shouldBe Kind.LINT
                String(env.payload) shouldBe "{\"problems\":[]}"
            }
        }
    }

    Given("a coverage kickoff request") {
        When("POSTing to /webhooks/coverage") {
            Then("it accepts and emits a COVERAGE envelope") {
                val c = Capture()
                testApplication {
                    application { webhookRoutes(c.ingest) }
                    val resp = client.post("/webhooks/coverage") { setBody("{\"report\":\"jacoco\"}") }
                    resp.status shouldBe HttpStatusCode.Accepted
                }
                c.env.shouldNotBeNull().kind shouldBe Kind.COVERAGE
            }
        }
    }

    Given("a GitHub event with a valid signature") {
        When("POSTing to /webhooks/github") {
            Then("it accepts and emits a CI envelope") {
                val c = Capture()
                val body = "{\"action\":\"completed\"}"
                testApplication {
                    application { webhookRoutes(c.ingest, secret = "topsecret") }
                    val resp = client.post("/webhooks/github") {
                        header("X-Hub-Signature-256", sign("topsecret", body))
                        setBody(body)
                    }
                    resp.status shouldBe HttpStatusCode.Accepted
                }
                c.env.shouldNotBeNull().kind shouldBe Kind.CI
            }
        }
    }

    Given("a GitHub event with an invalid signature") {
        When("POSTing to /webhooks/github") {
            Then("it is rejected with 401") {
                val c = Capture()
                testApplication {
                    application { webhookRoutes(c.ingest, secret = "topsecret") }
                    val resp = client.post("/webhooks/github") {
                        header("X-Hub-Signature-256", "sha256=deadbeef")
                        setBody("{}")
                    }
                    resp.status shouldBe HttpStatusCode.Unauthorized
                }
            }
        }
    }

    Given("a server with no configured secret") {
        When("POSTing to /webhooks/github") {
            Then("signature verification is skipped and it accepts") {
                val c = Capture()
                testApplication {
                    application { webhookRoutes(c.ingest) }
                    val resp = client.post("/webhooks/github") { setBody("{}") }
                    resp.status shouldBe HttpStatusCode.Accepted
                }
            }
        }
    }

    Given("an ingest function that fails") {
        When("POSTing a kickoff") {
            Then("it returns 500") {
                val c = Capture(fail = true)
                testApplication {
                    application { webhookRoutes(c.ingest) }
                    val resp = client.post("/webhooks/lint") { setBody("{}") }
                    resp.status shouldBe HttpStatusCode.InternalServerError
                }
            }
        }
    }

    Given("an oversized request body") {
        When("POSTing more than the 5 MiB cap to /webhooks/lint") {
            Then("it is rejected with 413 and never dispatched") {
                val c = Capture()
                val tooBig = "x".repeat(5 * 1024 * 1024 + 1)
                testApplication {
                    application { webhookRoutes(c.ingest) }
                    val resp = client.post("/webhooks/lint") { setBody(tooBig) }
                    resp.status shouldBe HttpStatusCode.PayloadTooLarge
                }
                c.env shouldBe null
            }
        }
    }

    Given("a health check") {
        When("GETting /healthz") {
            Then("it returns 200 ok") {
                testApplication {
                    application { webhookRoutes(Capture().ingest) }
                    val resp = client.get("/healthz")
                    resp.status shouldBe HttpStatusCode.OK
                    resp.bodyAsText() shouldBe "ok"
                }
            }
        }
    }

    Given("a wrong method on a known route") {
        When("GETting /webhooks/lint") {
            Then("it returns 405") {
                testApplication {
                    application { webhookRoutes(Capture().ingest) }
                    val resp = client.get("/webhooks/lint")
                    resp.status shouldBe HttpStatusCode.MethodNotAllowed
                }
            }
        }
    }
})
