/*
 * The debounce/coalesce transport hints for a synchronize review.
 *
 * Rapid pushes to one PR are collapsed so only the latest SHA is reviewed: a `synchronize` review is
 * enqueued with a debounce delay under a per-PR-per-window Cloud Tasks dedup name, so a burst of
 * pushes collapses to one delayed task. `opened`/`reopened`/`ready_for_review` enqueue immediately
 * (a human is waiting on the first review). Coalescing is a workflow concern, so it lives here rather
 * than in the transport (which stays dumb about PRs and SHAs).
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.PullRequestEvent
import com.automation.agent.githubapi.parsePullRequestEvent
import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.Kind
import com.automation.agent.tasks.EnqueueOptions
import java.math.BigInteger
import java.time.Instant
import java.util.Base64
import kotlin.time.Duration

/**
 * Nanoseconds between the proleptic-calendar zero instant (Jan 1, year 1 UTC) and the Unix epoch.
 * The debounce window is floored relative to that zero instant, not the Unix epoch, so the bucket
 * carried in the dedup name must be computed with the same origin to stay byte-identical across
 * every port (the name is a cross-port external contract).
 */
private val UNIX_TO_INTERNAL_NS: BigInteger =
    BigInteger.valueOf(62135596800L).multiply(BigInteger.valueOf(1_000_000_000L))

private val NS_PER_MS: BigInteger = BigInteger.valueOf(1_000_000L)
private val NS_PER_SECOND: BigInteger = BigInteger.valueOf(1_000_000_000L)

/**
 * Returns the transport hints for a review envelope so rapid pushes coalesce. A pull_request
 * "synchronize" (a new push to an open PR) is enqueued under a per-PR dedup name with a debounce
 * delay, so a burst of pushes collapses to one delayed task that reviews the latest SHA; the
 * worker's staleness check then enforces newest-wins. Any non-review kind, an unparseable payload,
 * or a non-positive debounce yields no options (immediate enqueue). Only the Cloud Tasks backend
 * honors the hints; the in-process backend ignores them.
 */
fun enqueueOptions(e: Envelope, debounce: Duration): EnqueueOptions {
    if (e.kind != Kind.REVIEW || debounce <= Duration.ZERO) return EnqueueOptions()
    val ev =
        try {
            parsePullRequestEvent(e.payload)
        } catch (_: IllegalArgumentException) {
            return EnqueueOptions()
        }
    if (ev.action != "synchronize") return EnqueueOptions()
    val bucket = truncateToWindow(e.receivedAt, debounce.inWholeMilliseconds)
    return EnqueueOptions(name = coalesceKey(ev, bucket), delay = debounce)
}

/**
 * The per-PR-per-window Cloud Tasks dedup name. Identically-named tasks collapse to one, so a burst
 * of pushes within a debounce window coalesces to a single review of the latest SHA.
 *
 * The name carries a time bucket (the receipt time floored to the debounce window) because Cloud
 * Tasks keeps a task name reserved for ~1h after the task completes or is deleted: a fixed per-PR
 * name would make a push that lands minutes after the previous review collide with the reserved name
 * and silently drop the new review. Bucketing gives each window a fresh name.
 *
 * The repo full name is base64url-encoded (unpadded) so the name is both valid in the Cloud Tasks
 * charset ([A-Za-z0-9_-]) and lossless: a naive replace-invalid-with-'-' would collapse distinct
 * repos (e.g. "acme/web.api" and "acme/web-api") to the same name and silently drop one PR's review.
 */
fun coalesceKey(ev: PullRequestEvent, bucketUnixNs: BigInteger): String {
    val encoded = Base64.getUrlEncoder().withoutPadding().encodeToString(ev.repoFullName.toByteArray(Charsets.UTF_8))
    return "review-$encoded-${ev.number}-$bucketUnixNs"
}

/**
 * Floors [at] to a multiple of [debounceMs] measured from the proleptic-calendar zero instant (see
 * [UNIX_TO_INTERNAL_NS]), returning the result as Unix nanoseconds. Computing the window origin this
 * way keeps the bucket byte-identical across every port.
 */
private fun truncateToWindow(at: Instant, debounceMs: Long): BigInteger {
    val unixNs = BigInteger.valueOf(at.epochSecond).multiply(NS_PER_SECOND).add(BigInteger.valueOf(at.nano.toLong()))
    val windowNs = BigInteger.valueOf(debounceMs).multiply(NS_PER_MS)
    if (windowNs <= BigInteger.ZERO) return unixNs
    return unixNs.subtract(unixNs.add(UNIX_TO_INTERNAL_NS).mod(windowNs))
}
