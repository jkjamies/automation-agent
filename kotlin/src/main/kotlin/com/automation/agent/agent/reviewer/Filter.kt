/*
 * The exclude-glob file filter that drops generated/vendored/binary churn and totals the filtered
 * patch bytes.
 *
 * Filtering first is the biggest cheap win: most "huge" PRs are mostly lockfile/vendor churn and
 * shrink to a handful of real files, so size is computed on the *filtered* set.
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.PRFile

/**
 * One compiled exclude glob. A pattern with no '/' matches against the file's basename (e.g.
 * "*.min.js", "go.sum"); a pattern with a '/' matches against the full path (e.g. a vendored tree).
 * "**" matches across path separators; "*" and "?" do not.
 *
 * Public so the standards-glob matcher (Standards.kt) can reuse the compiled-glob shape across the
 * module boundary rather than reaching into a private name.
 */
data class GlobPattern(val re: Regex, val basename: Boolean)

/** The kept (non-excluded) files and the total size of their patches in bytes. */
data class FilterResult(val kept: List<PRFile>, val diffBytes: Int)

/**
 * Drops changed files that are not worth reviewing — generated code, vendored trees, lockfiles,
 * minified bundles, snapshots, and binaries — before any size accounting or model call.
 */
class FileFilter(globs: List<String>) {
    private val patterns: List<GlobPattern> =
        globs.mapNotNull { raw ->
            val g = raw.trim()
            if (g.isEmpty()) null else GlobPattern(globToRegExp(g), !g.contains('/'))
        }

    /** Reports whether a path matches any exclude glob. */
    fun excluded(p: String): Boolean {
        val base = basename(p)
        for (pat in patterns) {
            val target = if (pat.basename) base else p
            if (pat.re.matches(target)) return true
        }
        return false
    }

    /**
     * Returns the kept (non-excluded) files and the total size of their patches in bytes. Size is
     * computed on the filtered set so the size gate sees real review surface, not churn; files whose
     * patch GitHub omitted are charged conservatively (see [patchBytes]) so an oversized PR cannot
     * undercount its way past the byte cap.
     */
    fun apply(files: List<PRFile>): FilterResult {
        val kept = mutableListOf<PRFile>()
        var diffBytes = 0
        for (fl in files) {
            if (excluded(fl.path)) continue
            kept.add(fl)
            diffBytes += patchBytes(fl)
        }
        return FilterResult(kept, diffBytes)
    }
}

/**
 * The per-changed-line byte estimate charged when GitHub omits a file's patch for an oversized text
 * diff. A unified-diff line is its content plus a one-char +/- prefix and a newline; real source
 * lines average well above this, so the estimate is deliberately conservative: the size gate must
 * over-, never under-, charge an omitted diff so a very large PR cannot slip the byte cap by
 * changing files too big for GitHub to diff.
 */
const val AVG_DIFF_LINE_BYTES = 50

/**
 * The diff-byte cost charged for one kept file. When GitHub returns the patch it is the exact byte
 * length. When GitHub omits it for an oversized text file (empty patch but non-zero line counts) it
 * is estimated from the reported additions+deletions. Binary files (no patch, no line counts) cost
 * nothing.
 */
fun patchBytes(fl: PRFile): Int {
    if (fl.patch.isNotEmpty()) return fl.patch.toByteArray(Charsets.UTF_8).size
    val lines = fl.additions + fl.deletions
    if (lines > 0) return lines * AVG_DIFF_LINE_BYTES
    return 0
}

private val METACHARS: Set<Char> = ".+()|[]{}^$\\".toSet()

/**
 * Compiles a glob into an anchored regexp. "**" becomes ".*" (crosses path separators), "*" becomes
 * "[^/]*" and "?" becomes "[^/]" (within one segment); every other regexp metacharacter is escaped
 * so it matches literally. Because all metacharacters are either escaped or rewritten, the result is
 * always a valid pattern.
 */
fun globToRegExp(glob: String): Regex {
    val b = StringBuilder()
    val n = glob.length
    var i = 0
    while (i < n) {
        val c = glob[i]
        when {
            c == '*' -> {
                if (i + 1 < n && glob[i + 1] == '*') {
                    b.append(".*")
                    i++ // consume the second '*'
                } else {
                    b.append("[^/]*")
                }
            }
            c == '?' -> b.append("[^/]")
            METACHARS.contains(c) -> b.append('\\').append(c)
            else -> b.append(c)
        }
        i++
    }
    return Regex(b.toString())
}

/** The final path segment (basename), splitting on '/' as posix paths do. */
private fun basename(p: String): String {
    val idx = p.lastIndexOf('/')
    return if (idx < 0) p else p.substring(idx + 1)
}
