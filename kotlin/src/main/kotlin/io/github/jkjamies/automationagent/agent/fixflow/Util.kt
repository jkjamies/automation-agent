package io.github.jkjamies.automationagent.agent.fixflow

/**
 * Returns the substring from the first '[' to the last ']', so a JSON array can be recovered from
 * model output that adds prose or code fences. Empty when no array is present.
 */
fun extractJsonArray(s: String): String {
    val i = s.indexOf('[')
    val j = s.lastIndexOf(']')
    return if (i < 0 || j < 0 || j < i) "" else s.substring(i, j + 1)
}

/** Returns the substring from the first '{' to the last '}'. Empty when no object is present. */
fun extractJsonObject(s: String): String {
    val i = s.indexOf('{')
    val j = s.lastIndexOf('}')
    return if (i < 0 || j < 0 || j < i) "" else s.substring(i, j + 1)
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
