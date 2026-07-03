/*
 * The build-agent split: pure ADK wiring (category + glue + distiller review agents, the prompt
 * loader, the JSON generate-content config). Logic lives in the sibling modules.
 *
 * The diff / standards are baked into each agent's system instruction because they are per-event;
 * the category and glue agents get the lazy `get_rule` tool when standards are present.
 *
 * ADK-Kotlin has no `LlmAgent.OutputKey`, so — rather than emulate it — each review lens is a code
 * agent that calls its tier model directly with the JSON generate-content config and emits its raw
 * findings text: a category lens writes to its own session-state key (read back by the parallel
 * fan-out); the glue lens emits its text as the event content (read back by the single-agent drive).
 * Because the lens drives the model itself (not through an ADK tool runner), the lazy `get_rule`
 * drill-down is served by a small in-lens tool loop rather than the runner's tool machinery.
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
import com.google.adk.kt.types.Content
import com.google.adk.kt.types.FunctionCall
import com.google.adk.kt.types.FunctionResponse
import com.google.adk.kt.types.GenerateContentConfig
import com.google.adk.kt.types.Part
import com.google.adk.kt.types.Role
import com.google.adk.kt.types.Tool
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow

private val prompts = Prompts.forAgent("reviewer")

// A lens loops at most this many model turns while it drills into standards rules via get_rule
// before the final findings text. A cap bounds a model that keeps calling the tool.
private const val MAX_TOOL_ITERS = 6

/**
 * Builds one category review agent: a code agent on the category's tier whose instruction is the
 * category prompt + the repo's standards rule menu (when any) + the filtered diff, writing its
 * findings JSON to the category's state key. When standards are present it also gets the lazy
 * get_rule tool.
 */
fun buildCategoryAgent(engine: Engine, c: Category, diff: String, std: Standards?): BaseAgent {
    val body = prompts.get(c.promptName)
    return CategoryReviewAgent(
        name = "review_${c.name}",
        description = "${c.title} review",
        model = modelForTier(engine, c.tier),
        instruction = buildReviewInstruction(body, diff, std),
        tools = standardsTools(std),
        stateKey = findingsKey(c.name),
    )
}

/**
 * Builds the glue/synthesis agent: a code-tier agent that sees the diff, the category findings so
 * far, and the repo's standards rule menu, emitting additional architectural-alignment /
 * testability / test-coverage findings (cross-lens dedup is done deterministically in code, not
 * here).
 */
fun buildGlueAgent(engine: Engine, diff: String, prior: List<Finding>, std: Standards?): BaseAgent {
    val body = prompts.get("glue")
    return GlueReviewAgent(
        name = "review_glue",
        description = "Holistic synthesis review",
        model = modelForTier(engine, Tier.CODE),
        instruction = buildGlueInstruction(body, diff, prior, std),
        tools = standardsTools(std),
    )
}

/**
 * Builds the standards distiller: a base-tier code agent (distillation is summarization/extraction,
 * the base tier) fed the reviewed repo's standards docs, prompted to emit a uniform tagged rule
 * list. It normalizes heterogeneous formats into one list.
 */
fun buildDistillerAgent(engine: Engine, docs: Map<String, String>, sources: List<String>): BaseAgent {
    val body = prompts.get("distill")
    val model = engine.baseLlm ?: throw IllegalStateException("reviewer: base model not configured")
    return DistillerAgent(
        name = "standards_distiller",
        description = "Distill the repo's standards docs into a tagged rule list",
        model = model,
        instruction = buildDistillerInstruction(body, docs, sources),
    )
}

/** A single category lens: generates its findings JSON and writes it to [stateKey] in session state. */
private class CategoryReviewAgent(
    name: String,
    description: String,
    private val model: Model,
    private val instruction: String,
    private val tools: List<RuleTool>,
    private val stateKey: String,
) : BaseAgent(name = name, description = description) {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> =
        flow {
            val raw = reviewGenerate(model, instruction, REVIEW_TRIGGER, tools)
            emit(textEvent(name, "reviewed", mapOf(stateKey to raw)))
        }
}

/** The holistic glue lens: generates its findings JSON and emits it as the event content. */
private class GlueReviewAgent(
    name: String,
    description: String,
    private val model: Model,
    private val instruction: String,
    private val tools: List<RuleTool>,
) : BaseAgent(name = name, description = description) {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> =
        flow {
            val raw = reviewGenerate(model, instruction, GLUE_TRIGGER, tools)
            emit(textEvent(name, raw))
        }
}

/** The standards distiller lens: generates the rule-list JSON and emits it as the event content. */
private class DistillerAgent(
    name: String,
    description: String,
    private val model: Model,
    private val instruction: String,
) : BaseAgent(name = name, description = description) {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> =
        flow {
            val raw = reviewGenerate(model, instruction, DISTILL_TRIGGER, emptyList())
            emit(textEvent(name, raw))
        }
}

/**
 * Runs a single lens against its tier model with the JSON generate-content config: [instruction] is
 * the lens prompt + standards menu + diff (the system instruction), [trigger] kicks generation, and
 * the concatenated text response is returned for the defensive parser to recover findings from.
 *
 * When [tools] is non-empty the lens serves the lazy get_rule drill-down inline: it forwards the
 * tool declarations, and on any function call it executes the matching tool, feeds the result back,
 * and re-drives — bounded by [MAX_TOOL_ITERS] — until the model emits its findings text.
 */
private suspend fun reviewGenerate(llm: Model, instruction: String, trigger: String, tools: List<RuleTool>): String {
    val toolByName = tools.associateBy { it.declaration.name }
    val toolConfig = if (tools.isEmpty()) null else listOf(Tool(functionDeclarations = tools.map { it.declaration }))
    val contents = mutableListOf(userText(trigger))
    var text = ""
    var iter = 0
    while (iter < MAX_TOOL_ITERS) {
        iter++
        val req =
            LlmRequest(
                contents = contents.toList(),
                config = GenerateContentConfig(systemInstruction = userText(instruction), responseMimeType = "application/json", tools = toolConfig),
            )
        val builder = StringBuilder()
        val calls = mutableListOf<FunctionCall>()
        llm.generateContent(req, stream = false).collect { resp ->
            resp.content?.parts?.forEach { p ->
                p.text?.let { builder.append(it) }
                p.functionCall?.let { calls.add(it) }
            }
        }
        text = builder.toString()
        if (calls.isEmpty() || toolByName.isEmpty()) return text
        // Record the model's call turn, then the tool responses, and drive again.
        contents.add(Content(role = Role.MODEL, parts = calls.map { Part(functionCall = it) }))
        val responseParts =
            calls.map { fc ->
                val result = toolByName[fc.name]?.execute?.invoke(fc.args ?: emptyMap()) ?: mapOf("error" to "unknown tool ${fc.name}")
                Part(functionResponse = FunctionResponse(name = fc.name, response = result))
            }
        contents.add(Content(role = Role.USER, parts = responseParts))
    }
    return text
}
