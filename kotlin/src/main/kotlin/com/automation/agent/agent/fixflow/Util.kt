package com.automation.agent.agent.fixflow

import kotlinx.serialization.SerializationException
import kotlinx.serialization.json.Json

/**
 * Returns the first complete JSON array in model output (which may add prose or code fences),
 * scanning from the first '[' — so trailing prose or a stray bracket can't corrupt the span.
 * Empty when none parses.
 */
fun extractJsonArray(s: String): String = firstJsonValue(s, '[', ']')

/** Returns the first complete JSON object in model output. Empty when none parses. */
fun extractJsonObject(s: String): String = firstJsonValue(s, '{', '}')

private fun firstJsonValue(s: String, open: Char, close: Char): String {
    var start = s.indexOf(open)
    while (start >= 0) {
        val end = matchingClose(s, start, open, close)
        if (end >= 0) {
            val candidate = s.substring(start, end + 1)
            try {
                Json.parseToJsonElement(candidate)
                return candidate
            } catch (_: SerializationException) {
                // balanced but not valid JSON; try the next opener
            }
        }
        start = s.indexOf(open, start + 1)
    }
    return ""
}

/** Index of the [close] that balances the [open] at [start] (string-aware), or -1. */
private fun matchingClose(s: String, start: Int, open: Char, close: Char): Int {
    var depth = 0
    var inStr = false
    var escaped = false
    for (i in start until s.length) {
        val c = s[i]
        if (inStr) {
            when {
                escaped -> escaped = false
                c == '\\' -> escaped = true
                c == '"' -> inStr = false
            }
            continue
        }
        when (c) {
            '"' -> inStr = true
            open -> depth++
            close -> if (--depth == 0) return i
        }
    }
    return -1
}

/** Removes surrounding markdown code fences a model may add and normalizes a trailing newline. */
fun stripFences(out: String): String {
    var s = out.trim()
    if (s.startsWith("```")) {
        val nl = s.indexOf('\n')
        if (nl >= 0) s = s.substring(nl + 1)
        val close = s.lastIndexOf("```")
        if (close >= 0) s = s.substring(0, close)
    }
    return s.trimEnd('\n') + "\n"
}
