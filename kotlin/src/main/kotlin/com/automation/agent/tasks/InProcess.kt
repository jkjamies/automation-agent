package com.automation.agent.tasks

import com.automation.agent.ingest.Envelope
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.job
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.selects.select
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeoutOrNull
import java.lang.System.Logger.Level
import kotlin.coroutines.cancellation.CancellationException

/** DEFAULT_MAX_CONCURRENT bounds in-flight in-process dispatches under burst (backpressure). */
const val DEFAULT_MAX_CONCURRENT = 32

/** DRAIN_TIMEOUT_MS caps how long close() waits for in-flight dispatches to finish. */
private const val DRAIN_TIMEOUT_MS = 15_000L

private val DEFAULT_LOG: System.Logger = System.getLogger("automation-agent.tasks")

/** Signals that enqueue was called after the transport began shutting down. */
class TransportClosedException : IllegalStateException("tasks: in-process transport is closed")

/**
 * Runs each envelope in a coroutine on a bounded pool — the local-dev and default backend.
 *
 * It reproduces the pre-transport behavior exactly: a burst applies backpressure (a bounded
 * permit pool), and a clean SIGTERM drains in-flight work via [close]. It does NOT survive an
 * instance being reclaimed mid-run, which is precisely why production uses the Cloud Tasks backend
 * instead. The [EnqueueOptions] hints are Cloud Tasks features and are ignored here (an immediate,
 * undeduplicated dispatch).
 */
class InProcess(
    private val dispatch: DispatchFunc,
    log: System.Logger? = null,
    maxConcurrent: Int = DEFAULT_MAX_CONCURRENT,
    // The drain budget; a test seam so the timeout path is exercisable without a real 15s wait.
    private val drainTimeoutMs: Long = DRAIN_TIMEOUT_MS,
) : Transport {
    // A null logger is ignored so the non-null default is preserved (the nil-logger guard).
    private val log: System.Logger = log ?: DEFAULT_LOG
    private val maxConcurrent = if (maxConcurrent < 1) DEFAULT_MAX_CONCURRENT else maxConcurrent

    // A burst blocks the enqueue caller (backpressure) instead of piling up detached coroutines. A
    // slot is taken by sending Unit and released by receiving — a Channel rather than a Semaphore so
    // the take can be raced against the close signal in a select (Semaphore.acquire has no select
    // clause), mirroring the Go reference's select over its slot channel and its closed channel.
    private val slots = Channel<Unit>(this.maxConcurrent)
    // Completed by close() to wake any enqueue caller parked waiting for a slot, so it fails promptly
    // with TransportClosedException once shutdown starts rather than waiting for a later release (the
    // Go reference races this as its closed channel inside Enqueue's select).
    private val closeSignal = CompletableDeferred<Unit>()
    // Owns the in-flight dispatch coroutines; close() drains them via the scope's children.
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    // Serializes the launch registration against close()'s drain snapshot. close() sets [closed]
    // and snapshots the children under this lock, and enqueue does its closed-recheck + launch
    // under it, so a launch either happens-before the snapshot (and is drained) or observes closed
    // and backs out — it can never slip past the drain (the mutex mirrors the Go reference's lock
    // serializing wg.Add against wg.Wait).
    private val mutex = Mutex()

    @Volatile
    private var closed = false

    /**
     * Dispatches [e] on the bounded pool. Suspends while the pool is full (backpressure under burst)
     * and otherwise returns immediately; the dispatch error is logged, not raised, because the
     * webhook response has already gone out. [opts] are ignored (Cloud Tasks features).
     *
     * @throws TransportClosedException if the transport has been closed (shutdown has begun).
     */
    override suspend fun enqueue(e: Envelope, opts: EnqueueOptions) {
        if (closed) throw TransportClosedException()
        // trySend takes a slot immediately when the pool has room. Only when it is full does the
        // caller park — surface that so sustained saturation is observable rather than a silently
        // delayed webhook ACK, then wait for a slot but race the close signal so a parked caller
        // wakes promptly (with TransportClosedException) once shutdown begins, instead of waiting
        // for a later release. This mirrors the Go reference selecting its slot channel against its
        // closed channel.
        if (slots.trySend(Unit).isFailure) {
            log.log(
                Level.WARNING,
                "dispatch concurrency saturated ($maxConcurrent in flight); " +
                    "webhook ingest is applying backpressure until a slot frees",
            )
            select {
                closeSignal.onAwait { throw TransportClosedException() }
                slots.onSend(Unit) {}
            }
        }
        // Hand the slot to the launched coroutine (which releases it when the dispatch finishes)
        // only once it is registered. If anything between here and a successful launch unwinds —
        // close() observed under the lock, or the caller being cancelled while suspended on the
        // mutex — release the slot here so it is never leaked.
        var launched = false
        try {
            // Recheck after the (possibly long) backpressure wait: close() may have begun while we
            // were parked on a slot. Without this, a dispatch could slip past the drain snapshot
            // and be abandoned on exit. Done under the same lock close() snapshots the children with.
            mutex.withLock {
                if (closed) throw TransportClosedException()
                scope.launch {
                    try {
                        dispatch(e)
                    } catch (ce: CancellationException) {
                        throw ce
                    } catch (ex: Exception) {
                        log.log(Level.ERROR, "dispatch failed kind=${e.kind} source=${e.source}", ex)
                    } finally {
                        slots.tryReceive() // release the slot (a buffered Unit is always present)
                    }
                }
                launched = true
            }
        } finally {
            if (!launched) slots.tryReceive()
        }
    }

    /**
     * Drains in-flight dispatches (bounded by [DRAIN_TIMEOUT_MS]) so a clean SIGTERM finishes work
     * in flight rather than abandoning it. On timeout it only stops waiting and does NOT cancel the
     * still-running dispatches, matching the reference backends, which let in-flight work run to
     * completion past the drain deadline.
     */
    override suspend fun close() {
        val children: List<Job> = mutex.withLock {
            closed = true
            closeSignal.complete(Unit) // wake any enqueue caller parked waiting for a slot
            scope.coroutineContext.job.children.toList()
        }
        if (children.isEmpty()) return
        log.log(Level.INFO, "draining ${children.size} in-flight dispatch(es)")
        if (withTimeoutOrNull(drainTimeoutMs) { children.joinAll() } == null) {
            log.log(Level.WARNING, "drain timed out; dispatch(es) still in flight")
        } else {
            log.log(Level.INFO, "drained in-flight work")
        }
    }
}
