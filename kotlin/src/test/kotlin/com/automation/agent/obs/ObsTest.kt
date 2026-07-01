/*
 * Tests for provider registration, the exporter selection, the sampler, and flush. Deterministic:
 * no live network, no LLM. A fake agent-shaped span tree stands in for the framework's native spans
 * so the assertions are on span names / attribute keys / structure, never on model output.
 */
package com.automation.agent.obs

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import io.kotest.matchers.types.shouldBeInstanceOf
import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.api.common.AttributeKey
import io.opentelemetry.exporter.logging.LoggingSpanExporter
import io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter
import io.opentelemetry.sdk.trace.samplers.Sampler

class ObsTest : BehaviorSpec({
    Given("the default (none) exporter") {
        When("initializing") {
            Then("it is a no-op: tracing stays disabled and shutdown is safe") {
                val shutdown = init(Config(exporter = EXPORTER_NONE))
                isEnabled() shouldBe false
                shutdown() // must not throw
            }
        }
    }

    Given("an unknown exporter") {
        When("initializing") {
            Then("it fails fast") {
                shouldThrow<IllegalArgumentException> { init(Config(exporter = "jaeger")) }
            }
        }
    }

    Given("a registered provider") {
        When("the framework emits its native span tree") {
            Then("the tree is exported with its GenAI attribute keys after a flush") {
                withRecording { exporter ->
                    val tracer = GlobalOpenTelemetry.getTracer("obs-test")
                    val agent = tracer.spanBuilder("invoke_agent automation_agent").startSpan()
                    val agentScope = agent.makeCurrent()
                    val llm = tracer.spanBuilder("call_llm gemma")
                        .setAttribute("gen_ai.request.model", "gemma")
                        .setAttribute("gen_ai.usage.input_tokens", 12L)
                        .startSpan()
                    val llmScope = llm.makeCurrent()
                    val tool = tracer.spanBuilder("execute_tool apply_fix")
                        .setAttribute("gen_ai.tool.name", "apply_fix")
                        .startSpan()
                    tool.end()
                    llmScope.close()
                    llm.end()
                    agentScope.close()
                    agent.end()

                    // Batch processor: nothing is exported until the flush.
                    exporter.finishedSpanItems shouldHaveSize 0
                    flush()

                    val spans = exporter.finishedSpanItems
                    spans shouldHaveSize 3
                    spans.map { it.name }.toSet() shouldBe setOf(
                        "invoke_agent automation_agent",
                        "call_llm gemma",
                        "execute_tool apply_fix",
                    )
                    // One trace, and the framework's GenAI attribute keys survive untouched.
                    spans.map { it.traceId }.toSet() shouldHaveSize 1
                    val llmSpan = spans.first { it.name == "call_llm gemma" }
                    llmSpan.attributes.get(AttributeKey.stringKey("gen_ai.request.model")) shouldBe "gemma"
                    llmSpan.attributes.get(AttributeKey.longKey("gen_ai.usage.input_tokens")) shouldBe 12L
                    val toolSpan = spans.first { it.name == "execute_tool apply_fix" }
                    toolSpan.attributes.get(AttributeKey.stringKey("gen_ai.tool.name")) shouldBe "apply_fix"
                }
            }
        }
    }

    Given("the sampler parser") {
        When("mapping standard values") {
            Then("known values map and unknown falls back to the parent-based default") {
                parseSampler("always_on") shouldBe Sampler.alwaysOn()
                parseSampler("always_off") shouldBe Sampler.alwaysOff()
                parseSampler("parentbased_always_off") shouldBe Sampler.parentBased(Sampler.alwaysOff())
                parseSampler("parentbased_always_on") shouldBe Sampler.parentBased(Sampler.alwaysOn())
                // Unknown falls back to the default rather than failing (advisory, not a gate).
                parseSampler("nonsense") shouldBe Sampler.parentBased(Sampler.alwaysOn())
            }
        }
    }

    Given("the OTLP header parser") {
        When("parsing the standard comma-separated form") {
            Then("it keeps valid pairs and drops keyless / empty ones") {
                val headers = parseOtlpHeaders("api-key=secret , env=prod,bad,=novalue,k=a=b")
                headers shouldBe mapOf("api-key" to "secret", "env" to "prod", "k" to "a=b")
            }
        }
    }

    Given("the exporter factory") {
        When("building console") {
            Then("it is a logging span exporter") {
                newExporter(Config(exporter = EXPORTER_CONSOLE)).shouldBeInstanceOf<LoggingSpanExporter>()
            }
        }
        When("building otlp with an endpoint") {
            Then("it is an OTLP/HTTP exporter") {
                newExporter(Config(exporter = EXPORTER_OTLP, otlpEndpoint = "http://localhost:4318"))
                    .shouldBeInstanceOf<OtlpHttpSpanExporter>()
            }
        }
        When("building otlp without an endpoint") {
            Then("it fails fast") {
                shouldThrow<IllegalArgumentException> { newExporter(Config(exporter = EXPORTER_OTLP)) }
            }
        }
        When("building gcp") {
            Then("it builds, or fails only for missing Application Default Credentials") {
                // The Cloud Trace exporter resolves ADC eagerly. With credentials present it builds;
                // without, it must fail for a credentials/project reason — never an unrelated error.
                val result = runCatching { newExporter(Config(exporter = EXPORTER_GCP)) }
                if (result.isFailure) {
                    val msg = (result.exceptionOrNull()?.message ?: "").lowercase()
                    val credentialsRelated = listOf("credential", "project", "default", "adc")
                        .any { msg.contains(it) }
                    credentialsRelated shouldBe true
                } else {
                    result.getOrNull().shouldNotBeNull()
                }
            }
        }
    }
})
