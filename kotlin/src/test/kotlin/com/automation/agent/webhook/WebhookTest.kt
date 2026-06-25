package com.automation.agent.webhook

import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.Kind
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

    Given("a lint kickoff with a configured secret") {
        When("POSTing to /webhooks/lint") {
            Then("an unsigned request is rejected and a signed one accepted") {
                val c = Capture()
                val body = "{\"problems\":[]}"
                testApplication {
                    application { webhookRoutes(c.ingest, secret = "topsecret") }
                    client.post("/webhooks/lint") { setBody(body) }.status shouldBe HttpStatusCode.Unauthorized
                    val ok = client.post("/webhooks/lint") {
                        header("X-Hub-Signature-256", sign("topsecret", body))
                        setBody(body)
                    }
                    ok.status shouldBe HttpStatusCode.Accepted
                }
                c.env.shouldNotBeNull().kind shouldBe Kind.LINT
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

    Given("internal routes with no token configured") {
        When("POSTing to them with a bearer") {
            Then("they are disabled (404) and nothing is dispatched") {
                val c = Capture()
                testApplication {
                    application { webhookRoutes(c.ingest) }
                    for (path in listOf("/internal/cron/daily", "/internal/sweep")) {
                        client.post(path) { header("Authorization", "Bearer x") }.status shouldBe HttpStatusCode.NotFound
                    }
                }
                c.env shouldBe null
            }
        }
    }

    Given("internal routes with a token") {
        When("POSTing without a bearer or with a wrong token") {
            Then("they return 401") {
                val c = Capture()
                testApplication {
                    application { webhookRoutes(c.ingest, internalToken = "sekret") }
                    client.post("/internal/cron/daily").status shouldBe HttpStatusCode.Unauthorized
                    client.post("/internal/sweep") {
                        header("Authorization", "Bearer wrong")
                    }.status shouldBe HttpStatusCode.Unauthorized
                }
                c.env shouldBe null
            }
        }

        When("POSTing the daily cron with a valid bearer") {
            Then("it accepts and emits a CRON_DAILY envelope") {
                val c = Capture()
                testApplication {
                    application { webhookRoutes(c.ingest, internalToken = "sekret") }
                    client.post("/internal/cron/daily") {
                        header("Authorization", "Bearer sekret")
                    }.status shouldBe HttpStatusCode.Accepted
                }
                val env = c.env.shouldNotBeNull()
                env.kind shouldBe Kind.CRON_DAILY
                env.source shouldBe "internal:/cron/daily"
            }
        }
    }

    Given("the internal sweep endpoint") {
        When("the sweep succeeds") {
            Then("it returns 200 and runs the handler") {
                var swept = false
                testApplication {
                    application { webhookRoutes(Capture().ingest, internalToken = "sekret", sweep = SweepFunc { swept = true }) }
                    client.post("/internal/sweep") {
                        header("Authorization", "Bearer sekret")
                    }.status shouldBe HttpStatusCode.OK
                }
                swept shouldBe true
            }
        }

        When("the sweep handler throws") {
            Then("it returns 500") {
                testApplication {
                    application { webhookRoutes(Capture().ingest, internalToken = "sekret", sweep = SweepFunc { throw RuntimeException("boom") }) }
                    client.post("/internal/sweep") {
                        header("Authorization", "Bearer sekret")
                    }.status shouldBe HttpStatusCode.InternalServerError
                }
            }
        }

        When("no sweep handler is configured") {
            Then("it returns 501") {
                testApplication {
                    application { webhookRoutes(Capture().ingest, internalToken = "sekret") }
                    client.post("/internal/sweep") {
                        header("Authorization", "Bearer sekret")
                    }.status shouldBe HttpStatusCode.NotImplemented
                }
            }
        }
    }
})
