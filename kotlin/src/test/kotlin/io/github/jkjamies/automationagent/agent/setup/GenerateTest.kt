package io.github.jkjamies.automationagent.agent.setup

import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow

/** A deterministic [Model] that emits fixed text or throws. */
private class FixedModel(val text: String? = null, val error: Throwable? = null) : Model {
    override val name: String = "fixed"

    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> = flow {
        error?.let { throw it }
        emit(LlmResponse(content = assistantText(text ?: "")))
    }
}

class GenerateTest : BehaviorSpec({
    Given("a model that returns fixed text") {
        When("generating text") {
            Then("the concatenated response is returned") {
                generateText(FixedModel(text = "the answer"), "be terse", "question?") shouldBe "the answer"
            }
        }
    }

    Given("a model that errors") {
        When("generating text") {
            Then("the error propagates") {
                shouldThrow<RuntimeException> {
                    generateText(FixedModel(error = RuntimeException("model down")), "s", "u")
                }
            }
        }
    }
})
