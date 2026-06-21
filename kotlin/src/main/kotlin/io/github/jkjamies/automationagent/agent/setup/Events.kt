package io.github.jkjamies.automationagent.agent.setup

import com.google.adk.kt.events.Event
import com.google.adk.kt.events.EventActions
import com.google.adk.kt.types.Content
import com.google.adk.kt.types.Part
import com.google.adk.kt.types.Role

/**
 * Small content/event helpers over the ADK-Kotlin genai-like types. Code agents use these to seed
 * invocations, read text back, and emit state.
 */

/** Builds a user-role content message from plain text — the common way to seed an invocation. */
fun userText(text: String): Content = Content(role = Role.USER, parts = listOf(Part(text = text)))

/** Builds a model-role content message from plain text. */
fun assistantText(text: String): Content = Content(role = Role.MODEL, parts = listOf(Part(text = text)))

/** Concatenates the text parts of a content (null-safe). */
fun contentText(content: Content?): String =
    content?.parts?.mapNotNull { it.text }?.joinToString("") ?: ""

/** Returns the concatenated text of the final content in a list, or "". */
fun lastText(contents: List<Content>): String = contentText(contents.lastOrNull())

/**
 * Builds an [Event] carrying model-authored text, optionally with a state delta. Code agents use
 * this to emit output and write workflow state. An empty/absent [state] leaves the delta empty
 * (ADK-Kotlin's delta is a non-null map).
 */
fun textEvent(author: String, text: String, state: Map<String, Any>? = null): Event {
    val actions =
        if (state.isNullOrEmpty()) {
            EventActions()
        } else {
            EventActions(stateDelta = state.toMutableMap())
        }
    return Event(author = author, content = assistantText(text), actions = actions)
}

/**
 * Returns the string value at [key] in session state, or "" if absent or not a string. Accepts any
 * read-only map view of state (ADK-Kotlin's `State` implements `Map<String, Any>`).
 */
fun stateString(state: Map<String, Any?>, key: String): String = state[key] as? String ?: ""
