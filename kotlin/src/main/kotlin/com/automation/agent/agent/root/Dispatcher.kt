/*
 * Package root is the dispatcher kicked off for every ingest. It routes a normalized
 * ingest.Envelope to the right workflow by Kind. Keeping a single entry point is why "root"
 * exists: new ingress sources (GitHub/Jira/Confluence/human) and smarter (e.g. LLM-based) routing
 * slot in here without restructuring.
 */
package com.automation.agent.agent.root

import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.Kind

/** Runs the work for one ingest envelope. Failures throw. */
fun interface Handler {
    suspend operator fun invoke(envelope: Envelope)
}

/** Routes envelopes to handlers by [Kind]. */
class Dispatcher(log: System.Logger? = null) {
    private val log: System.Logger = log ?: System.getLogger("automation-agent.root")
    private val handlers = mutableMapOf<Kind, Handler>()

    /** Binds a handler to a kind (last registration wins). */
    fun register(kind: Kind, handler: Handler) {
        handlers[kind] = handler
    }

    /** Reports whether a kind has a registered handler. */
    fun handles(kind: Kind): Boolean = handlers.containsKey(kind)

    /**
     * Routes one envelope. An unregistered kind is logged and ignored, so an ingress that isn't
     * wired yet is a no-op, not a crash.
     */
    suspend fun dispatch(envelope: Envelope) {
        val handler = handlers[envelope.kind]
        if (handler == null) {
            log.log(System.Logger.Level.WARNING, "no handler for ingest kind ${envelope.kind} (source ${envelope.source})")
            return
        }
        log.log(System.Logger.Level.INFO, "dispatching kind=${envelope.kind} source=${envelope.source}")
        handler(envelope)
    }
}
