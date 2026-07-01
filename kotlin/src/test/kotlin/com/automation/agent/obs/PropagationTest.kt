/*
 * Tests for backend-aware trace-context propagation. Deterministic: no network. The Cloud Tasks half
 * is a header round-trip; the in-process half is the coroutine-context passthrough. Both resolve to
 * the same logical trace, and injection adds nothing when tracing is off.
 */
package com.automation.agent.obs

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.maps.shouldContainKey
import io.kotest.matchers.shouldBe
import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.api.trace.Span
import io.opentelemetry.context.Context
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking

class PropagationTest : BehaviorSpec({
    Given("an active trace and the Cloud Tasks header seam") {
        When("injecting then extracting") {
            Then("the round-trip continues the same trace as a remote child") {
                withRecording {
                    val ingress = GlobalOpenTelemetry.getTracer("obs-test").spanBuilder("ingress").startSpan()
                    val carrier = inject(mutableMapOf(), Context.current().with(ingress))
                    ingress.end()

                    carrier shouldContainKey "traceparent"

                    val extracted = extract(carrier)
                    val child = GlobalOpenTelemetry.getTracer("obs-test")
                        .spanBuilder("dispatch").setParent(extracted).startSpan()
                    child.end()

                    child.spanContext.traceId shouldBe ingress.spanContext.traceId
                }
            }
        }
    }

    Given("an active trace and the in-process passthrough seam") {
        When("a detached coroutine launches with the captured context element") {
            Then("the coroutine continues the ingress trace with no carrier") {
                withRecording {
                    val ingress = GlobalOpenTelemetry.getTracer("obs-test").spanBuilder("ingress").startSpan()
                    val scope = ingress.makeCurrent()
                    // Capture while the ingress span is current (as the transport does at enqueue).
                    val element = currentTraceContextElement()
                    scope.close()
                    ingress.end()

                    var childTraceId = ""
                    runBlocking {
                        launch(element) {
                            // The launched coroutine sees the ingress trace as current.
                            childTraceId = Span.current().spanContext.traceId
                        }
                    }
                    childTraceId shouldBe ingress.spanContext.traceId
                }
            }
        }
    }

    Given("tracing is disabled") {
        When("injecting") {
            Then("no traceparent is added, so nothing leaks onto a task") {
                // No provider installed: the global propagator is a no-op.
                GlobalOpenTelemetry.resetForTest()
                val carrier = inject()
                carrier.isEmpty() shouldBe true
            }
        }
    }
})
