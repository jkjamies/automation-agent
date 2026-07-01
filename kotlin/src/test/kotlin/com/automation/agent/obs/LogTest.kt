/*
 * Tests for log <-> trace correlation. Deterministic: a capturing logger records the forwarded
 * messages. Under an active span the wrapped logger appends trace_id / span_id; with no active span
 * the message is passed through untouched.
 */
package com.automation.agent.obs

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import io.opentelemetry.api.GlobalOpenTelemetry
import java.util.ResourceBundle

/** A minimal capturing [System.Logger] that records the last message it was handed. */
private class CapturingLogger : System.Logger {
    var last: String? = null

    override fun getName(): String = "capturing"
    override fun isLoggable(level: System.Logger.Level): Boolean = true
    override fun log(level: System.Logger.Level, bundle: ResourceBundle?, msg: String?, thrown: Throwable?) {
        last = msg
    }
    override fun log(level: System.Logger.Level, bundle: ResourceBundle?, format: String?, vararg params: Any?) {
        last = format
    }
}

class LogTest : BehaviorSpec({
    Given("the wrapped logger and an active span") {
        When("a record is emitted under the span") {
            Then("it carries the span's trace_id and span_id") {
                withRecording {
                    val base = CapturingLogger()
                    val logger = newLogHandler(base)
                    val span = GlobalOpenTelemetry.getTracer("obs-test").spanBuilder("work").startSpan()
                    val scope = span.makeCurrent()
                    try {
                        logger.log(System.Logger.Level.INFO, null as ResourceBundle?, "started", null as Throwable?)
                    } finally {
                        scope.close()
                        span.end()
                    }

                    val message = base.last ?: ""
                    message shouldContain "started"
                    message shouldContain "trace_id=${span.spanContext.traceId}"
                    message shouldContain "span_id=${span.spanContext.spanId}"
                }
            }
        }
    }

    Given("the wrapped logger and an active span, using the parameterized overload") {
        When("a formatted record is emitted under the span") {
            Then("the trace ids are appended after the format text") {
                withRecording {
                    val base = CapturingLogger()
                    val logger = newLogHandler(base)
                    val span = GlobalOpenTelemetry.getTracer("obs-test").spanBuilder("work").startSpan()
                    val scope = span.makeCurrent()
                    try {
                        logger.log(System.Logger.Level.WARNING, "count={0}", 5)
                    } finally {
                        scope.close()
                        span.end()
                    }
                    val message = base.last ?: ""
                    message shouldContain "count={0}"
                    message shouldContain "trace_id=${span.spanContext.traceId}"
                }
            }
        }
    }

    Given("the wrapped logger and no active span") {
        When("a record is emitted") {
            Then("the message is passed through untouched") {
                GlobalOpenTelemetry.resetForTest()
                val base = CapturingLogger()
                val logger = newLogHandler(base)
                logger.log(System.Logger.Level.INFO, null as ResourceBundle?, "plain", null as Throwable?)
                base.last shouldBe "plain"
            }
        }
    }
})
