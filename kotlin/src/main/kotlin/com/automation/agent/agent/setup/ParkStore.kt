/*
 * The park store: the durable seam that replaces the in-memory parked-run registry.
 *
 * A [ParkRecord] is the per-run state a suspended fix loop needs to resume — which session, which
 * long-running call, how many attempts, and the serialized run params — keyed by a globally unique
 * session id and indexed by PR key (`owner/repo#pr`) while parked. The store provides single-winner
 * claim semantics ([ParkStore.resolveByPrKey], [ParkStore.sweep]) so exactly one of {CI webhook,
 * soft timer, sweep} ever resolves a given run.
 *
 * Backends: `memory` (in-process, the default; a restart drops parked runs), `sqlite`, and
 * `firestore`. The park store is our own concept (no ADK type) and is always hand-rolled. This
 * file holds the interface, the memory backend, and the [newParkStore] factory; the durable
 * backends live in sibling files and are wired into the factory.
 */
package com.automation.agent.agent.setup

import com.automation.agent.config.Config
import com.automation.agent.config.SessionBackend
import java.io.File
import java.time.Instant

/**
 * The per-run state of one suspended fix loop. [prKey] is empty until the run parks on CI (it is
 * the resume index, `owner/repo#pr`); [params] is the caller's opaque serialized run inputs;
 * [parkedAt] is when the run parked (the sweep cutoff reads it), null until parked.
 */
data class ParkRecord(
    /** Globally unique session id (a UUID); stable from kickoff through every retry. */
    val sessionId: String,
    /** Resume index `owner/repo#pr`; empty until the run parks awaiting CI. */
    val prKey: String = "",
    /** The parked long-running call id (ADK), fed back on resume. */
    val callId: String = "",
    /** Attempt count (caller-tracked, not GitHub's). */
    val attempts: Int = 0,
    /** Opaque caller-serialized run params (see the Driver's RunParams). */
    val params: String = "",
    /** When the run parked (UTC); null until parked. The sweep cutoff compares against it. */
    val parkedAt: Instant? = null,
)

/** Whether a record is currently parked (indexed for resume). */
fun ParkRecord.isParked(): Boolean = prKey.isNotEmpty()

/**
 * Persists parked-run state with single-winner claim semantics.
 *
 * Implementations must make [resolveByPrKey] and [sweep] atomic: for a given PR key (or stale
 * record) exactly one concurrent caller wins the claim and the rest get nothing, so a CI webhook,
 * a soft timer, and the periodic sweep never double-resolve a run. All methods are `suspend` so
 * the durable backends can do I/O.
 */
interface ParkStore {
    /** Create or replace the record for `record.sessionId`, (re)indexing by PR key. */
    suspend fun put(record: ParkRecord)

    /** Return the record for [sessionId], or null if it is not stored. */
    suspend fun get(sessionId: String): ParkRecord?

    /**
     * Atomically claim the run indexed by [prKey]: clear the index and return the record (the
     * per-run record is retained so a retry can read its params). Returns null for a
     * late/duplicate/unknown caller or an empty key. The returned record carries the claimed PR
     * key so the caller can stop its timer.
     */
    suspend fun resolveByPrKey(prKey: String): ParkRecord?

    /** Remove the record (and any lingering index) for [sessionId]. Terminal cleanup; no-op if absent. */
    suspend fun delete(sessionId: String)

    /**
     * Atomically claim and return every parked record whose [ParkRecord.parkedAt] is before
     * [cutoff]. Each is claimed exactly once (no double-winner); the returned records keep their
     * PR key.
     */
    suspend fun sweep(cutoff: Instant): List<ParkRecord>

    /** How many records are currently parked (PR-key-indexed). */
    suspend fun parkedCount(): Int

    /**
     * Releases backing resources (durable backends). Not `suspend`: it is a blocking
     * connection/client close called once on clean shutdown, so it needs no coroutine bridge.
     */
    fun close()
}

/**
 * In-process park store (the default backend). Holds records by session id and a PR-key →
 * session-id index of the parked subset. Records are immutable data classes, so a caller can never
 * corrupt stored state by holding a reference; claims are single-winner because the index lookup
 * and its removal happen under one lock. A restart drops everything (the documented memory trade).
 */
class MemoryParkStore : ParkStore {
    private val lock = Any()
    private val bySession = mutableMapOf<String, ParkRecord>()
    private val index = mutableMapOf<String, String>() // prKey -> sessionId

    override suspend fun put(record: ParkRecord): Unit =
        synchronized(lock) {
            // Stale-index hygiene: if this session was indexed under a different key, drop the old
            // entry so a re-park under a new key cannot leave the prior key dangling.
            val prev = bySession[record.sessionId]
            if (prev != null && prev.prKey.isNotEmpty() && prev.prKey != record.prKey &&
                index[prev.prKey] == record.sessionId
            ) {
                index.remove(prev.prKey)
            }
            // One active record per PR key: if a different session currently owns this key, un-park
            // it so the index has a single winner (mirrors the durable backends' duplicate clear).
            // Otherwise resolve/sweep could return either session, and a later delete of the
            // displaced session would strand this one.
            if (record.prKey.isNotEmpty()) {
                val owner = index[record.prKey]
                if (owner != null && owner != record.sessionId) {
                    bySession[owner]?.let { bySession[owner] = it.copy(prKey = "") }
                }
            }
            bySession[record.sessionId] = record
            if (record.prKey.isNotEmpty()) index[record.prKey] = record.sessionId
        }

    override suspend fun get(sessionId: String): ParkRecord? = synchronized(lock) { bySession[sessionId] }

    override suspend fun resolveByPrKey(prKey: String): ParkRecord? =
        synchronized(lock) {
            if (prKey.isEmpty()) return@synchronized null
            val sid = index[prKey] ?: return@synchronized null
            index.remove(prKey) // claim
            val rec = bySession[sid] ?: return@synchronized null
            bySession[sid] = rec.copy(prKey = "") // unpark in storage; retain so a retry can read params
            rec.copy(prKey = prKey) // hand the claimed key back so the caller can stop its timer
        }

    override suspend fun delete(sessionId: String): Unit =
        synchronized(lock) {
            val rec = bySession[sessionId]
            if (rec != null && rec.prKey.isNotEmpty() && index[rec.prKey] == sessionId) {
                index.remove(rec.prKey)
            }
            bySession.remove(sessionId)
        }

    override suspend fun sweep(cutoff: Instant): List<ParkRecord> =
        synchronized(lock) {
            val claimed = mutableListOf<ParkRecord>()
            for ((key, sid) in index.entries.toList()) {
                val rec = bySession[sid]
                if (rec?.parkedAt == null || !rec.parkedAt.isBefore(cutoff)) continue
                index.remove(key) // claim
                claimed += rec.copy(prKey = key) // keep the key for logging/cleanup
                bySession[sid] = rec.copy(prKey = "") // unpark in storage; retain until the caller clears it
            }
            claimed
        }

    override suspend fun parkedCount(): Int = synchronized(lock) { index.size }

    override fun close() {
        // No backing resources for the in-memory store.
    }
}

/** Build the park store for the configured session backend. */
fun newParkStore(cfg: Config): ParkStore =
    when (cfg.sessionBackend) {
        SessionBackend.MEMORY -> MemoryParkStore()
        // Absolute path so the park store and the session service open the very same file.
        SessionBackend.SQLITE -> SqliteParkStore(File(cfg.sqliteDsn).absolutePath)
        SessionBackend.FIRESTORE -> FirestoreParkStore(cfg.firestoreProject, "${cfg.firestoreCollection}_parked_runs")
    }
