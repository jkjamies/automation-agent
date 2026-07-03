/*
 * The hidden fingerprint marker and the pure reconcile: given this run's inline findings + the PR's
 * existing comments, what to post vs minimize.
 *
 * The marker tags each inline comment with the fingerprint of the finding that produced it, so a
 * later re-review re-identifies the comment from GitHub itself (GitHub-as-store — no local durable
 * state). It is an HTML comment appended to the body and is an external-ish contract: keep the exact
 * format stable across ports.
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.ReviewCommentRef

private const val FP_MARKER_PREFIX = "<!-- ar-fp:"
private const val FP_MARKER_SUFFIX = " -->"

// Extracts the fingerprint from a comment body. Non-greedy so a body with trailing content still
// matches only the marker payload.
private val FP_MARKER_PATTERN = Regex("<!-- ar-fp:(.+?) -->")

/** Renders the hidden fingerprint marker appended to an inline comment body. */
fun fpMarker(fingerprint: String): String = FP_MARKER_PREFIX + fingerprint + FP_MARKER_SUFFIX

/**
 * Returns the fingerprint embedded in a comment body, or "" if it carries none — a foreign comment,
 * or one posted before reconciliation existed.
 */
fun parseFpMarker(body: String): String = FP_MARKER_PATTERN.find(body)?.groupValues?.get(1) ?: ""

/**
 * The outcome of comparing this run's inline findings against the comments already on the PR: which
 * findings to post fresh, and which existing comments to minimize.
 */
data class ReconcileResult(
    val toPost: List<Finding>, // inline findings with no comment yet
    val toMinimize: List<String>, // node ids of comments whose finding is gone
)

/**
 * Compares this run's inline findings to the PR's existing fingerprinted review comments
 * (GitHub-as-store). A finding already represented by a comment is kept — not re-posted, so a
 * re-review is idempotent; a finding with no existing comment is posted; an existing fingerprinted
 * comment with no matching finding this run is minimized as outdated. Comments without our marker
 * (foreign, or pre-reconciliation) are ignored. [ReconcileResult.toMinimize] is sorted for
 * deterministic behavior and tests.
 */
fun reconcile(findings: List<Finding>, existing: List<ReviewCommentRef>): ReconcileResult {
    val current = findings.map { fingerprint(it) }.toSet()
    val have = linkedMapOf<String, MutableList<String>>() // fingerprint -> existing node ids
    for (rc in existing) {
        val fp = parseFpMarker(rc.body)
        if (fp != "") have.getOrPut(fp) { mutableListOf() }.add(rc.nodeId)
    }

    val toPost = mutableListOf<Finding>()
    for (f in findings) {
        if (fingerprint(f) !in have) toPost.add(f)
    }
    val toMinimize = mutableListOf<String>()
    for ((fp, ids) in have) {
        if (fp !in current) toMinimize.addAll(ids)
    }
    toMinimize.sort()
    return ReconcileResult(toPost = toPost, toMinimize = toMinimize)
}
