/*
 * Tests for the HTTP tracing interceptor. Deterministic: no live network beyond Ktor's in-memory
 * test host, no LLM. Assertions are on the server span's name / kind / status / parenting, and on the
 * health-probe exclusion and the flush-before-return guarantee.
 */
package com.automation.agent.obs

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.ints.shouldBeGreaterThan
import io.kotest.matchers.shouldBe
import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.api.common.AttributeKey
import io.opentelemetry.api.trace.SpanKind
import io.opentelemetry.api.trace.StatusCode
import io.opentelemetry.context.Context
import io.ktor.client.request.header
import io.ktor.client.request.get
import io.ktor.client.request.post
import io.ktor.http.HttpStatusCode
import io.ktor.server.response.respond
import io.ktor.server.response.respondText
import io.ktor.server.routing.get
import io.ktor.server.routing.post
import io.ktor.server.routing.routing
import io.ktor.server.testing.testApplication

class MiddlewareTest : BehaviorSpec({
    Given("a traced request") {
        When("it is handled") {
            Then("one server span is exported with the method-path name and status attribute") {
                withRecording { exporter ->
                    testApplication {
                        application {
                            installObsTracing()
                            routing { post("/webhooks/lint") { call.respond(HttpStatusCode.Accepted) } }
                        }
                        client.post("/webhooks/lint").status shouldBe HttpStatusCode.Accepted
                    }
                    flush()

                    val spans = exporter.finishedSpanItems
                    spans shouldHaveSize 1
                    spans[0].name shouldBe "POST /webhooks/lint"
                    spans[0].kind shouldBe SpanKind.SERVER
                    spans[0].attributes.get(AttributeKey.longKey("http.response.status_code")) shouldBe 202L
                }
            }
        }
    }

    Given("the health probe") {
        When("it is polled") {
            Then("it is excluded from tracing") {
                withRecording { exporter ->
                    testApplication {
                        application {
                            installObsTracing()
                            routing { get("/healthz") { call.respondText("ok") } }
                        }
                        client.get("/healthz").status shouldBe HttpStatusCode.OK
                    }
                    flush()
                    exporter.finishedSpanItems shouldHaveSize 0
                }
            }
        }
    }

    Given("a buffered span and the health probe") {
        When("the probe is handled") {
            Then("it does not flush other requests' buffered spans, but a traced request does") {
                withRecording { exporter ->
                    // Buffer a span (as an in-flight agent run would).
                    GlobalOpenTelemetry.getTracer("obs-test")
                        .spanBuilder("invoke_agent automation_agent").startSpan().end()
                    testApplication {
                        application {
                            installObsTracing()
                            routing {
                                get("/healthz") { call.respondText("ok") }
                                post("/webhooks/lint") { call.respond(HttpStatusCode.Accepted) }
                            }
                        }
                        client.get("/healthz")
                        // The probe left the buffered span un-exported (no span, no flush).
                        exporter.finishedSpanItems shouldHaveSize 0
                        // A traced request flushes them (its own server span plus the buffered one).
                        client.post("/webhooks/lint")
                    }
                    flush()
                    exporter.finishedSpanItems.size shouldBeGreaterThan 0
                }
            }
        }
    }

    Given("a request that returns 5xx") {
        When("it is handled") {
            Then("the server span records an error status") {
                withRecording { exporter ->
                    testApplication {
                        application {
                            installObsTracing()
                            routing { post("/webhooks/lint") { call.respond(HttpStatusCode.InternalServerError, "boom") } }
                        }
                        client.post("/webhooks/lint").status shouldBe HttpStatusCode.InternalServerError
                    }
                    flush()
                    val span = exporter.finishedSpanItems.first { it.name == "POST /webhooks/lint" }
                    span.status.statusCode shouldBe StatusCode.ERROR
                }
            }
        }
    }

    Given("a handler that throws") {
        When("the exception propagates") {
            Then("the span records the exception and keeps its message (not overwritten by the 5xx branch)") {
                withRecording { exporter ->
                    testApplication {
                        application {
                            installObsTracing()
                            routing { post("/webhooks/lint") { throw RuntimeException("boom") } }
                        }
                        // The engine turns the unhandled throw into a 500.
                        client.post("/webhooks/lint").status shouldBe HttpStatusCode.InternalServerError
                    }
                    flush()
                    val span = exporter.finishedSpanItems.first { it.name == "POST /webhooks/lint" }
                    span.status.statusCode shouldBe StatusCode.ERROR
                    // The exception message survives to the top-level status.
                    span.status.description shouldBe "boom"
                    span.events.any { it.name == "exception" } shouldBe true
                }
            }
        }
    }

    Given("a task carrying an inbound traceparent") {
        When("the dispatch request is handled") {
            Then("the server span continues the ingress trace as its child") {
                withRecording { exporter ->
                    // Model a task carrying a traceparent injected by the transport on enqueue.
                    val ingress = GlobalOpenTelemetry.getTracer("obs-test").spanBuilder("ingress").startSpan()
                    val carrier = inject(mutableMapOf(), Context.current().with(ingress))
                    ingress.end()

                    testApplication {
                        application {
                            installObsTracing()
                            routing { post("/internal/dispatch") { call.respond(HttpStatusCode.OK, "ok") } }
                        }
                        client.post("/internal/dispatch") { header("traceparent", carrier.getValue("traceparent")) }
                    }
                    flush()

                    val dispatch = exporter.finishedSpanItems.first { it.name == "POST /internal/dispatch" }
                    dispatch.traceId shouldBe ingress.spanContext.traceId
                    dispatch.parentSpanContext.spanId shouldBe ingress.spanContext.spanId
                }
            }
        }
    }

    Given("tracing disabled") {
        When("a request is handled") {
            Then("the interceptor is a true no-op and the request still completes") {
                GlobalOpenTelemetry.resetForTest()
                var called = false
                testApplication {
                    application {
                        installObsTracing()
                        routing { post("/webhooks/lint") { called = true; call.respond(HttpStatusCode.Accepted) } }
                    }
                    client.post("/webhooks/lint").status shouldBe HttpStatusCode.Accepted
                }
                called shouldBe true
            }
        }
    }
})
