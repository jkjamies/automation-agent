/*
 * The build-agent split: pure ADK wiring (category + glue review agents, the prompt loader, the JSON
 * generate-content config). Logic lives in the sibling modules.
 *
 * The diff is baked into each agent's system instruction because it is per-event.
 *
 * ADK-Kotlin has no `LlmAgent.OutputKey`, so — rather than emulate it — each review lens is a code
 * agent that calls its tier model directly with the JSON generate-content config and emits its raw
 * findings text: a category lens writes to its own session-state key (read back by the parallel
 * fan-out); the glue lens emits its text as the event content (read back by the single-agent drive).
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.agent.setup.Prompts
import com.automation.agent.agent.setup.contentText
import com.automation.agent.agent.setup.textEvent
import com.automation.agent.agent.setup.userText
import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.agents.InvocationContext
import com.google.adk.kt.events.Event
import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.Model
import com.google.adk.kt.types.GenerateContentConfig
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.flow.fold

private val prompts = Prompts.forAgent("reviewer")

/**
 * Builds one category review agent: a code agent on the category's tier whose instruction is the
 * category prompt + the filtered diff, writing its findings JSON to the category's state key.
 */
fun buildCategoryAgent(engine: Engine, c: Category, diff: String): BaseAgent {
    val body = prompts.get(c.promptName)
    return CategoryReviewAgent(
        name = "review_${c.name}",
        description = "${c.title} review",
        model = modelForTier(engine, c.tier),
        instruction = buildReviewInstruction(body, diff),
        stateKey = findingsKey(c.name),
    )
}

/**
 * Builds the glue/synthesis agent: a code-tier agent that sees the diff and the category findings so
 * far, emitting additional architectural-alignment / testability / test-coverage findings (cross-lens
 * dedup is done deterministically in code, not here).
 */
fun buildGlueAgent(engine: Engine, diff: String, prior: List<Finding>): BaseAgent {
    val body = prompts.get("glue")
    return GlueReviewAgent(
        name = "review_glue",
        description = "Holistic synthesis review",
        model = modelForTier(engine, Tier.CODE),
        instruction = buildGlueInstruction(body, diff, prior),
    )
}

/** A single category lens: generates its findings JSON and writes it to [stateKey] in session state. */
private class CategoryReviewAgent(
    name: String,
    description: String,
    private val model: Model,
    private val instruction: String,
    private val stateKey: String,
) : BaseAgent(name = name, description = description) {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> =
        flow {
            val raw = reviewGenerate(model, instruction, REVIEW_TRIGGER)
            emit(textEvent(name, "reviewed", mapOf(stateKey to raw)))
        }
}

/** The holistic glue lens: generates its findings JSON and emits it as the event content. */
private class GlueReviewAgent(
    name: String,
    description: String,
    private val model: Model,
    private val instruction: String,
) : BaseAgent(name = name, description = description) {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> =
        flow {
            val raw = reviewGenerate(model, instruction, GLUE_TRIGGER)
            emit(textEvent(name, raw))
        }
}

/**
 * Runs a single non-streaming completion with the JSON generate-content config: [instruction] is the
 * lens prompt + diff (the system instruction), [trigger] kicks generation, and the concatenated text
 * response is returned for the defensive parser to recover findings from.
 */
private suspend fun reviewGenerate(llm: Model, instruction: String, trigger: String): String {
    val req =
        LlmRequest(
            contents = listOf(userText(trigger)),
            config = GenerateContentConfig(systemInstruction = userText(instruction), responseMimeType = "application/json"),
        )
    return llm.generateContent(req, stream = false).fold(StringBuilder()) { sb, resp ->
        sb.append(contentText(resp.content))
    }.toString()
}
