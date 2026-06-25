package com.automation.agent.agent.setup

import com.automation.agent.config.Config
import com.automation.agent.config.Provider
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe

// A baseline config with the package defaults (ollama, gemma4:12b). Overridden per-case via copy().
private val baseConfig: Config = Config.loadFrom { null }

class LlmTest : BehaviorSpec({
    Given("an Ollama provider config") {
        When("building the default LLM") {
            Then("it returns a model named after the configured tag") {
                val m = buildLLM(baseConfig.copy(llmProvider = Provider.OLLAMA, ollamaModel = "gemma4:12b"))
                m.name shouldBe "gemma4:12b"
            }
        }
    }

    Given("a Gemini provider config with no model") {
        When("building the LLM") {
            Then("it fails fast, since GEMINI_MODEL is required") {
                shouldThrow<IllegalArgumentException> {
                    buildLLM(baseConfig.copy(llmProvider = Provider.GEMINI, geminiModel = ""))
                }
            }
        }
    }

    // An "unknown provider" string is not tested here: the provider is a sealed enum that the
    // config layer validates on load (see ConfigSpec), so an unknown value cannot reach buildLLM
    // and the `when` is exhaustive without a default branch.
})
