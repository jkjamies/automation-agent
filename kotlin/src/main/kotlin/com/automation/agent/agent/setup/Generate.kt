package com.automation.agent.agent.setup

import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.Model
import com.google.adk.kt.types.GenerateContentConfig
import kotlinx.coroutines.flow.fold

/**
 * Runs a single non-streaming completion: [system] is the instruction, [user] is the prompt, and
 * the concatenated text response is returned. It lets callers outside this package use a [Model]
 * without touching the genai types directly.
 *
 * A failing model throws from the underlying [Flow]; the exception propagates out of this suspend
 * function.
 */
suspend fun generateText(llm: Model, system: String, user: String): String {
    val req =
        LlmRequest(
            contents = listOf(userText(user)),
            config = GenerateContentConfig(systemInstruction = userText(system)),
        )
    return llm.generateContent(req, stream = false).fold(StringBuilder()) { sb, resp ->
        sb.append(contentText(resp.content))
    }.toString()
}
