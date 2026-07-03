/*
 * Which head-side lines of a patch GitHub accepts an inline comment on, used to route a finding to
 * an inline comment vs. the summary's out-of-diff section.
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.PRFile

/**
 * Returns the new-side (head) line numbers in a unified-diff patch that GitHub will accept a
 * RIGHT-side inline comment on: added ('+') and context (' ') lines. Removed ('-') lines have no
 * head-side line and are skipped. A malformed or empty patch yields an empty set, so a finding on
 * it is treated as out-of-diff rather than posted at a wrong line.
 */
fun commentableLines(patch: String): Set<Int> {
    val out = mutableSetOf<Int>()
    var newLine = 0
    var inHunk = false
    for (line in patch.split("\n")) {
        if (line.startsWith("@@")) {
            val parsed = parseHunkNewStart(line)
            newLine = parsed.start
            inHunk = parsed.ok
            continue
        }
        if (!inHunk) continue
        when {
            line.startsWith("+") -> {
                out.add(newLine)
                newLine += 1
            }
            line.startsWith("-") -> Unit // removed line: advances the old side only, no head-side line
            line.startsWith(" ") -> {
                out.add(newLine)
                newLine += 1
            }
            line.startsWith("\\") -> Unit // "\ No newline at end of file": metadata, not a line
            else -> inHunk = false // a blank or unexpected line ends this hunk's body
        }
    }
    return out
}

/** The parsed start line of a hunk header plus whether it parsed. */
private data class HunkStart(val start: Int, val ok: Boolean)

/**
 * Parses the new-file starting line from a hunk header "@@ -a,b +c,d @@", returning
 * `HunkStart(c, true)`. A header it cannot parse yields `HunkStart(0, false)` so the body until the
 * next header is skipped rather than mis-numbered.
 */
private fun parseHunkNewStart(header: String): HunkStart {
    val plus = header.indexOf('+')
    if (plus < 0) return HunkStart(0, false)
    var rest = header.substring(plus + 1)
    val end = indexAny(rest, " ,")
    if (end >= 0) rest = rest.substring(0, end)
    val n = rest.toIntOrNull() ?: return HunkStart(0, false)
    if (n <= 0) return HunkStart(0, false)
    return HunkStart(n, true)
}

/** Returns the index of the first character in [s] that is in [chars], or -1. */
private fun indexAny(s: String, chars: String): Int {
    for (i in s.indices) {
        if (chars.contains(s[i])) return i
    }
    return -1
}

/** Maps each changed file to the head-side lines an inline comment can target. */
class DiffIndex(files: List<PRFile>) {
    private val idx: Map<String, Set<Int>> = files.associate { it.path to commentableLines(it.patch) }

    /** Reports whether file:line falls on a commentable head-side line of the diff. */
    fun inDiff(file: String, line: Int): Boolean {
        val lines = idx[file] ?: return false
        return line in lines
    }
}
