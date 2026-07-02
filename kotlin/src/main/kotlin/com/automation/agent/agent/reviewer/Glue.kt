/*
 * The deterministic verify-gate and cross-lens merge logic the glue/synthesis pass owns.
 *
 * The glue *agent* itself (architectural alignment, testability, and test-coverage reasoning) is
 * wired in AgentsSetup and run in Review; cross-lens dedup and the confidence gate are done here in
 * code rather than asked of the model, so they are deterministic and unit-testable.
 */
package com.automation.agent.agent.reviewer

/**
 * Removes findings below the configured minimum confidence (the phase-1 verify gate). A non-positive
 * minimum keeps everything. Never aliases the caller's list.
 */
fun dropLowConfidence(findings: List<Finding>, minimum: Double): List<Finding> {
    if (minimum <= 0.0) return findings.toList()
    return findings.filter { it.confidence >= minimum }
}

/**
 * Collapses findings that share a fingerprint (same file+line+message, across lenses), keeping the
 * one with the worst severity (ties broken by higher confidence). Input order is otherwise
 * preserved.
 */
fun dedupe(findings: List<Finding>): List<Finding> {
    val seen = mutableMapOf<String, Int>() // fingerprint -> index in out
    val out = mutableListOf<Finding>()
    for (f in findings) {
        val fp = fingerprint(f)
        val i = seen[fp]
        if (i != null) {
            if (better(f, out[i])) out[i] = f
            continue
        }
        seen[fp] = out.size
        out.add(f)
    }
    return out
}

/**
 * Reports whether `a` should replace `b` among duplicates: worse severity wins; on a tie, higher
 * confidence.
 */
private fun better(a: Finding, b: Finding): Boolean {
    val ra = severityRank(a.severity)
    val rb = severityRank(b.severity)
    if (ra != rb) return ra > rb
    return a.confidence > b.confidence
}

/**
 * Forces every finding to nitpick severity. The catch-all "(other)" category is intentionally
 * low-signal, so its findings are demoted rather than allowed to drive the scorecard.
 */
fun demoteToNitpick(findings: List<Finding>): List<Finding> = findings.map { it.copy(severity = Severity.NITPICK) }
