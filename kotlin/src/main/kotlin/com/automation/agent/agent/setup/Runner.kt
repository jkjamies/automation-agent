package com.automation.agent.agent.setup

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.runners.InMemoryRunner
import com.google.adk.kt.runners.Runner
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.flow.fold

/**
 * In-memory runner helpers. They drive a workflow agent to completion locally and in tests. Errors
 * surface as exceptions thrown from the underlying `Flow<Event>`.
 */

/** Builds an in-memory runner rooted at [root], suitable for driving a workflow agent. */
fun newRunner(appName: String, root: BaseAgent): Runner = InMemoryRunner(agent = root, appName = appName)

/**
 * Runs the agent for a single input, draining events. Side-effecting agents (e.g. a notifier)
 * perform their work as they run; the first error propagates.
 */
suspend fun drive(runner: Runner, userId: String, sessionId: String, input: String) {
    runner.runAsync(userId, sessionId, newMessage = userText(input)).collect { }
}

/**
 * Runs the agent and returns the concatenated text of its non-partial responses. For a tool-using
 * agent this is the final answer after any tool calls (intermediate function-call/response events
 * carry no text).
 */
suspend fun driveText(runner: Runner, userId: String, sessionId: String, input: String): String =
    runner.runAsync(userId, sessionId, newMessage = userText(input))
        .fold(StringBuilder()) { sb, ev ->
            if (!ev.partial) sb.append(contentText(ev.content))
            sb
        }.toString()

/**
 * Runs the agent and accumulates every state delta emitted by its events into a single map. Useful
 * for fan-out workflows where parallel sub-agents each write a distinct state key the caller reads
 * back.
 */
suspend fun driveCollectState(runner: Runner, userId: String, sessionId: String, input: String): Map<String, Any> {
    val state = mutableMapOf<String, Any>()
    runner.runAsync(userId, sessionId, newMessage = userText(input)).collect { ev ->
        state.putAll(ev.actions.stateDelta)
    }
    return state
}
