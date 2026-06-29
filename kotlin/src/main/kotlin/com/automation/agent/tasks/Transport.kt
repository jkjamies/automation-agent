/*
 * Package tasks is the execution transport between webhook ingress and the dispatcher. Webhook
 * ingress reduces a request to an ingest.Envelope and calls Transport.enqueue, which returns fast;
 * the envelope's workflow runs later — in a background coroutine for the in-process backend, or in
 * a fresh /internal/dispatch request delivered by Cloud Tasks in production. The seam exists
 * because on Cloud Run with request-based billing CPU is throttled to near-zero once a response is
 * sent, so multi-minute LLM compute must run *inside* a request (Cloud Tasks gives that, plus
 * durable retry and rate limiting). See specs/20260626-workflow-execution-transport.md.
 * Deterministic tooling — no agent imports (the dispatcher is injected as a DispatchFunc).
 */
package com.automation.agent.tasks

import com.automation.agent.ingest.Envelope
import kotlin.time.Duration

/**
 * Runs the work for one envelope. It is the root dispatcher's dispatch, passed in so this package
 * stays decoupled from the agent layer. A thrown exception is a transient failure the caller
 * surfaces so the queue retries.
 */
fun interface DispatchFunc {
    suspend operator fun invoke(e: Envelope)
}

/**
 * Optional per-enqueue hints. The transport stays deliberately dumb about workflow semantics: it
 * carries these to the backend but does not interpret them. Coalesce-to-latest / staleness logic
 * lives in the workflow, not here (spec Decision §3). Only the Cloud Tasks backend honors them.
 */
data class EnqueueOptions(
    // name is a dedup key. Cloud Tasks drops a duplicate task with the same name for ~1h, giving
    // idempotency against a redelivered webhook. Null means no dedup.
    val name: String? = null,
    // delay schedules delivery this far in the future (e.g. a review debounce window). Zero means
    // deliver immediately.
    val delay: Duration = Duration.ZERO,
)

/**
 * Enqueues an envelope for asynchronous execution and returns quickly. A thrown exception becomes a
 * 500 to the webhook caller (so GitHub/Cloud Scheduler retries).
 */
interface Transport {
    /** Schedules [e] for execution. [opts] carry optional, backend-honored hints. */
    suspend fun enqueue(e: Envelope, opts: EnqueueOptions = EnqueueOptions())

    /**
     * Releases the backend: the in-process backend drains in-flight coroutines; the Cloud Tasks
     * backend closes its client. Safe to call once at shutdown.
     */
    suspend fun close()
}
