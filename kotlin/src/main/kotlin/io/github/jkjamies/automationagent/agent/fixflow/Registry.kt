package io.github.jkjamies.automationagent.agent.fixflow

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlin.time.Duration

/**
 * One suspended fix run awaiting its CI result. It lives only in memory: if the process restarts,
 * parked runs are lost and their PRs are abandoned (an accepted trade — see the architecture
 * notes). [sessionId] + [callId] are what a resume needs to feed the CI outcome back into the run.
 */
internal class ParkedRun(
    val sessionId: String,
    val callId: String,
    val attempts: Int = 0,
) {
    /** The per-run timeout job, cancelled on resolve or re-park. */
    var timer: Job? = null
}

/**
 * Tracks parked runs in memory, keyed by PR. Exactly one of {CI webhook, timeout timer} ever
 * resolves a given run: [resolve] atomically removes the entry, so late or duplicate deliveries
 * (and a timer firing the same instant a webhook lands) find nothing and no-op. The registry IS the
 * in-flight record — no DB, no PR scan. [scope] runs the per-run timeout timers.
 */
internal class RunRegistry(private val scope: CoroutineScope) {
    private val lock = Any()
    private val runs = mutableMapOf<String, ParkedRun>()

    /**
     * Records a parked run for [prKey] and arms its timeout. [onTimeout] fires once if the run is
     * still parked when [timeout] elapses; it must call [resolve] to claim the run (and will lose
     * the claim if a webhook got there first).
     */
    fun park(prKey: String, run: ParkedRun, timeout: Duration, onTimeout: suspend (String) -> Unit) {
        synchronized(lock) {
            runs[prKey]?.timer?.cancel() // replace any prior parking for this PR (e.g. a retry re-park)
            run.timer = scope.launch { delay(timeout); onTimeout(prKey) }
            runs[prKey] = run
        }
    }

    /**
     * Atomically claims and removes the parked run for [prKey], cancelling its timer. Returns the
     * run for the single winner; null for late/duplicate callers.
     */
    fun resolve(prKey: String): ParkedRun? =
        synchronized(lock) {
            val run = runs.remove(prKey) ?: return null
            run.timer?.cancel()
            run
        }

    /** The number of currently parked runs. */
    fun size(): Int = synchronized(lock) { runs.size }
}
