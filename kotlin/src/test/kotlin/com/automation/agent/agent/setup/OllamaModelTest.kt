package com.automation.agent.agent.setup

import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import com.google.adk.kt.types.FinishReason
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respond
import io.ktor.client.engine.mock.respondError
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.http.HttpStatusCode
import io.ktor.http.headersOf
import io.ktor.serialization.kotlinx.json.json
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.emptyFlow
import kotlinx.coroutines.flow.toList
import kotlinx.serialization.json.Json

private fun ndjsonClient(body: String): HttpClient {
    val engine = MockEngine { respond(body, HttpStatusCode.OK, headersOf("Content-Type", "application/x-ndjson")) }
    return HttpClient(engine) { install(ContentNegotiation) { json(Json { ignoreUnknownKeys = true }) } }
}

private fun errorClient(): HttpClient {
    val engine = MockEngine { respondError(HttpStatusCode.InternalServerError, "boom") }
    return HttpClient(engine) { install(ContentNegotiation) { json(Json { ignoreUnknownKeys = true }) } }
}

class OllamaModelTest : BehaviorSpec({
    Given("a streaming Ollama response of two chunks") {
        When("generating with stream=true") {
            Then("partials are emitted before a complete final response") {
                val body =
                    """
                    {"model":"gemma","message":{"role":"assistant","content":"Hello "},"done":false}
                    {"model":"gemma","message":{"role":"assistant","content":"world"},"done":true,"done_reason":"stop"}
                    """.trimIndent()
                val m = OllamaModel("http://ollama.test", "gemma", ndjsonClient(body))
                val resps = m.generateContent(LlmRequest(contents = listOf(userText("hi"))), stream = true).toList()

                val partials = resps.filter { it.partial }.map { contentText(it.content) }
                partials shouldBe listOf("Hello ")
                val final = resps.last { !it.partial }
                contentText(final.content) shouldBe "Hello world"
                final.finishReason shouldBe FinishReason.STOP
            }
        }
    }

    Given("a single non-streaming Ollama response") {
        When("generating with stream=false") {
            Then("exactly one complete response comes back") {
                val body = """{"model":"gemma","message":{"role":"assistant","content":"Full answer"},"done":true,"done_reason":"stop"}"""
                val m = OllamaModel("http://ollama.test", "gemma", ndjsonClient(body))
                val resps = m.generateContent(LlmRequest(contents = listOf(userText("hi"))), stream = false).toList()

                resps.size shouldBe 1
                contentText(resps[0].content) shouldBe "Full answer"
                resps[0].partial shouldBe false
                resps[0].finishReason shouldBe FinishReason.STOP
            }
        }
    }

    Given("a stream that ends without a done=true chunk") {
        When("generating with stream=true") {
            Then("a terminal response is still emitted from the accumulated content") {
                val body =
                    """
                    {"model":"gemma","message":{"role":"assistant","content":"Hello "},"done":false}
                    {"model":"gemma","message":{"role":"assistant","content":"world"},"done":false}
                    """.trimIndent()
                val m = OllamaModel("http://ollama.test", "gemma", ndjsonClient(body))
                val resps = m.generateContent(LlmRequest(contents = listOf(userText("hi"))), stream = true).toList()

                val final = resps.last { !it.partial }
                contentText(final.content) shouldBe "Hello world"
                final.finishReason shouldBe FinishReason.STOP
            }
        }
    }

    Given("an Ollama server returning 500") {
        When("generating") {
            Then("the error surfaces from the flow") {
                val m = OllamaModel("http://ollama.test", "gemma", errorClient())
                shouldThrow<Exception> {
                    m.generateContent(LlmRequest(), stream = false).toList()
                }
            }
        }
    }

    Given("the model constructor") {
        When("given an empty model tag") {
            Then("it fails fast") {
                shouldThrow<IllegalArgumentException> { OllamaModel("http://localhost:11434", "") }
            }
        }
    }

    Given("a request whose model overrides the default tag") {
        When("resolving the model name") {
            Then("the request model wins, else the default tag is used") {
                val override = object : Model {
                    override val name: String = "override"
                    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> = emptyFlow()
                }
                val m = OllamaModel("http://localhost:11434", "default-model")
                m.modelName(LlmRequest(model = override)) shouldBe "override"
                m.modelName(LlmRequest()) shouldBe "default-model"
            }
        }
    }
})
