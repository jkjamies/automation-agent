package io.github.jkjamies.automationagent.agent.setup

import com.google.adk.kt.models.Gemini
import com.google.adk.kt.models.Model
import io.github.jkjamies.automationagent.config.Config
import io.github.jkjamies.automationagent.config.Provider

/**
 * The LLM provider switch. Agents depend only on the returned [Model] interface, so switching
 * providers is a config change, not a code change (see .agents/standards/architecture-design.md §4). This is the only
 * package permitted to import provider SDKs — enforced by the :konsist arch rules.
 *
 * No `context` is threaded: ADK-Kotlin is coroutine-based, so cancellation rides on the calling
 * coroutine's scope.
 */

/** Returns the default [Model] (triage, explore, summary) for the configured provider. */
fun buildLLM(cfg: Config): Model = buildLLM(cfg, cfg.ollamaModel, cfg.geminiModel)

/**
 * Returns the [Model] for the code-change steps (lint rewrite, coverage test generation) —
 * typically a larger model. Falls back to the default model when no code model is configured
 * (the config layer already applies that fallback).
 */
fun buildCodeLLM(cfg: Config): Model = buildLLM(cfg, cfg.ollamaCodeModel, cfg.geminiCodeModel)

private fun buildLLM(cfg: Config, ollamaModel: String, geminiModel: String): Model =
    when (cfg.llmProvider) {
        Provider.OLLAMA -> OllamaModel(cfg.ollamaHost, ollamaModel)
        Provider.GEMINI -> newGeminiModel(geminiModel)
    }

/**
 * Builds the Gemini-backed [Model] for the cloud deployment. Credentials/backend are read from the
 * environment by the GenAI SDK (`GOOGLE_API_KEY`/`GEMINI_API_KEY`, or Vertex via
 * `GOOGLE_GENAI_USE_VERTEXAI`/`GOOGLE_CLOUD_PROJECT`) when no explicit key is passed.
 */
internal fun newGeminiModel(geminiModel: String): Model {
    require(geminiModel.isNotEmpty()) { "GEMINI_MODEL must be set when LLM_PROVIDER=gemini" }
    return Gemini(name = geminiModel)
}
