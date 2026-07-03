/* The two-dimensional file-count / diff-byte size cap. */
package com.automation.agent.agent.reviewer

/** The outcome of the size gate: a deny [reason] (phrased for the "please split" notice) and the flag. */
data class Oversize(val reason: String, val denied: Boolean)

/**
 * Reports whether a filtered diff exceeds either configured cap. The gate is two-dimensional: a PR
 * is too large if it changes more than [maxFiles] files OR its filtered patches exceed
 * [maxDiffBytes] — review-or-deny, no degrade tier. A non-positive cap disables that dimension. The
 * reason is phrased for the "too large — please split" deny comment. The size is taken on the
 * *filtered* set, so excluded lockfile/vendor churn never trips the gate.
 */
fun oversize(fileCount: Int, diffBytes: Int, maxFiles: Int, maxDiffBytes: Int): Oversize {
    if (maxFiles > 0 && fileCount > maxFiles) {
        return Oversize(
            "$fileCount changed files (after excluding generated files) exceeds the $maxFiles-file review limit",
            true,
        )
    }
    if (maxDiffBytes > 0 && diffBytes > maxDiffBytes) {
        return Oversize(
            "$diffBytes diff bytes (after excluding generated files) exceeds the $maxDiffBytes-byte review limit",
            true,
        )
    }
    return Oversize("", false)
}
