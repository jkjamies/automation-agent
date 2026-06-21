@file:OptIn(ExperimentalResumabilityFeature::class)

package io.github.jkjamies.automationagent.agent.setup

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.agents.ResumabilityConfig
import com.google.adk.kt.annotations.ExperimentalResumabilityFeature
import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import com.google.adk.kt.runners.InMemoryRunner
import com.google.adk.kt.runners.Runner
import com.google.adk.kt.types.Content
import com.google.adk.kt.types.FinishReason
import com.google.adk.kt.types.FunctionCall
import com.google.adk.kt.types.FunctionResponse
import com.google.adk.kt.types.Part
import com.google.adk.kt.types.Role
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow

/**
 * Generic ADK long-running suspend/resume plumbing. It lives in `agent.setup` because it touches
 * the genai types; callers (e.g. `fixflow`) stay genai-free.
 *
 * In ADK-Kotlin, suspend/resume is the **resumability** feature: a [Runner] built with
 * `ResumabilityConfig(isResumable = true)` parks on a long-running tool call and resumes when a
 * matching function response is fed back on the same in-memory session. See
 * [[kotlin-adk-model-api-verified]].
 */

/**
 * The outcome of driving a long-running agent through one cycle: until it suspends on a
 * long-running tool call, or finishes without parking.
 *
 * @property parkedCallId the id of the long-running call the agent suspended on, or `null` when the
 *   run finished instead of parking.
 * @property toolResponses each tool name mapped to its most recent response this cycle. A tool whose
 *   handler errored surfaces here with an `"error"` key (ADK converts a handler error into
 *   `{"error": message}` rather than aborting the run).
 * @property final the concatenated text of the agent's non-partial responses.
 */
data class DriveResult(
    val parkedCallId: String? = null,
    val toolResponses: Map<String, Map<String, Any?>> = emptyMap(),
    val final: String = "",
)

/**
 * Drives a long-running agent through ADK's suspend/resume on a single in-memory session. It is
 * the generic plumbing behind a CI-wait loop: all domain policy (what to apply, whether to retry,
 * how long to wait) lives in the caller; this type only knows how to run-to-park and
 * resume-with-a-result.
 */
class LongRunDriver(private val runner: Runner, private val userId: String) {
    /** Seeds a fresh invocation on [sessionId] with [input] and drives until the agent parks or finishes. */
    suspend fun start(sessionId: String, input: String): DriveResult = drive(sessionId, userText(input))

    /**
     * Feeds the real result for a parked long-running call ([toolName] + [callId]) back into
     * [sessionId] and drives until the agent re-parks or finishes. Valid only on a session a prior
     * start/resume parked; a stale callId resolves to a benign no-op run.
     */
    suspend fun resume(
        sessionId: String,
        callId: String,
        toolName: String,
        response: Map<String, Any?>,
    ): DriveResult {
        val content =
            Content(
                role = Role.USER,
                parts = listOf(Part(functionResponse = FunctionResponse(id = callId, name = toolName, response = response))),
            )
        return drive(sessionId, content)
    }

    private suspend fun drive(sessionId: String, input: Content): DriveResult {
        var parkedCallId: String? = null
        val toolResponses = mutableMapOf<String, Map<String, Any?>>()
        val final = StringBuilder()
        runner.runAsync(userId, sessionId, newMessage = input).collect { ev ->
            ev.longRunningToolIds.firstOrNull()?.let { parkedCallId = it }
            ev.content?.parts?.forEach { p ->
                p.functionResponse?.let { toolResponses[it.name] = it.response }
            }
            if (!ev.partial) final.append(contentText(ev.content))
        }
        return DriveResult(parkedCallId = parkedCallId, toolResponses = toolResponses, final = final.toString())
    }

    companion object {
        /**
         * Builds a driver over [root], sharing one resumable in-memory session service so a resume
         * lands on the same suspended run a start parked.
         */
        fun create(appName: String, userId: String, root: BaseAgent): LongRunDriver {
            val runner =
                InMemoryRunner(
                    agent = root,
                    appName = appName,
                    resumabilityConfig = ResumabilityConfig(isResumable = true),
                )
            return LongRunDriver(runner, userId)
        }
    }
}

/**
 * Configures a deterministic two-phase long-running loop driven by [newSequencerModel]: call
 * [action] (a normal tool), then [wait] (a long-running tool that suspends the run). When the run
 * resumes with [wait]'s real result, [retryWhen] decides whether to loop (call [action] again) or
 * conclude.
 *
 * @property wait the long-running tool that parks the run awaiting an external result. It is called
 *   with the [action]'s result map as its args, so the wait tool's argument type must accept those
 *   fields (extra fields are rejected by strict schema validation).
 * @property retryWhen reports whether a resumed wait result warrants another action. `null` means
 *   never retry. Policy that needs out-of-band state (attempt counts, deadlines) belongs in the
 *   caller, which simply declines to resume when it does not want a loop.
 */
data class SequencerConfig(
    val action: String,
    val wait: String,
    val retryWhen: ((waitResponse: Map<String, Any?>) -> Boolean)? = null,
)

/**
 * Returns a [Model] that emits a fixed action→wait tool sequence instead of reasoning. It carries
 * no policy: the caller owns retry/stop/timeout and only resumes a parked run when it wants another
 * attempt. The substantive LLM work happens inside the action tool's own handler (which may drive
 * real sub-agents).
 */
fun newSequencerModel(cfg: SequencerConfig): Model = Sequencer(cfg)

internal class Sequencer(private val cfg: SequencerConfig) : Model {
    override val name: String = "sequencer:${cfg.action}+${cfg.wait}"

    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> = flow {
        emit(decide(request.contents))
    }

    /**
     * Chooses the next step from the most recent function response in history:
     *  - none yet                 -> call action
     *  - action returned an error -> conclude (nothing to wait on)
     *  - action returned a result -> call wait, forwarding the result as its args
     *  - wait result, retryWhen   -> call action again
     *  - wait result, otherwise   -> conclude
     */
    internal fun decide(contents: List<Content>): LlmResponse {
        val last = lastFunctionResponse(contents)
        return when {
            last == null -> call(cfg.action, emptyMap(), contents)
            last.name == cfg.action ->
                if (last.response.containsKey("error")) {
                    sequencerText("${cfg.action} failed: ${last.response["error"]}")
                } else {
                    call(cfg.wait, last.response, contents)
                }
            last.name == cfg.wait ->
                if (cfg.retryWhen?.invoke(last.response) == true) call(cfg.action, emptyMap(), contents) else sequencerText("done")
            else -> sequencerText("done")
        }
    }

    private fun call(name: String, args: Map<String, Any?>, contents: List<Content>): LlmResponse {
        // Unique id per call so the flow correlates each long-running park independently across
        // retries within one session.
        val id = "${name}_${countFunctionCalls(contents, name) + 1}"
        val fc = FunctionCall(id = id, name = name, args = args)
        return LlmResponse(
            content = Content(role = Role.MODEL, parts = listOf(Part(functionCall = fc))),
            finishReason = FinishReason.STOP,
        )
    }

    private fun sequencerText(text: String): LlmResponse =
        LlmResponse(content = assistantText(text), finishReason = FinishReason.STOP)
}

private fun lastFunctionResponse(contents: List<Content>): FunctionResponse? =
    contents.flatMap { it.parts }.mapNotNull { it.functionResponse }.lastOrNull()

private fun countFunctionCalls(contents: List<Content>, name: String): Int =
    contents.flatMap { it.parts }.count { it.functionCall?.name == name }
