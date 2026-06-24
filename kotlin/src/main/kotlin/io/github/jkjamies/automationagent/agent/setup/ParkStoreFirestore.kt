/*
 * The firestore park store: the serverless, scale-to-zero [ParkStore] backend.
 *
 * Park records are documents keyed by session id; `pr_key` doubles as the resume index ('' when not
 * parked), so re-parking under a new key cannot leak a stale entry. The atomic claim
 * ([resolveByPrKey]) runs in a firestore transaction: of N concurrent resolvers the first to commit
 * clears `pr_key`; the others retry, re-read the cleared key, and find nothing — so exactly one
 * wins. Doc field names are snake_case to match the Go reference's schema. This file is exercised
 * only under the firestore emulator (see the emulator-gated tests) and excluded from the coverage
 * floor. Mirrors the Go/JS firestore park stores.
 */
package io.github.jkjamies.automationagent.agent.setup

import com.google.cloud.firestore.DocumentSnapshot
import com.google.cloud.firestore.Firestore
import com.google.cloud.firestore.FirestoreOptions
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.time.Instant

/** A durable park store backed by Cloud Firestore. */
class FirestoreParkStore(project: String, private val collection: String) : ParkStore {
    // An empty project lets the client detect it from ADC / GOOGLE_CLOUD_PROJECT / the emulator env.
    private val db: Firestore =
        FirestoreOptions.newBuilder().apply { if (project.isNotEmpty()) setProjectId(project) }.build().service

    private fun col() = db.collection(collection)

    override suspend fun put(record: ParkRecord): Unit = withContext(Dispatchers.IO) {
        if (record.prKey.isNotEmpty()) {
            // One active doc per pr_key: clear it on any OTHER session still holding it, so
            // resolve/sweep have a single winner. Best-effort (not transactional with the set).
            val dupes = col().whereEqualTo("pr_key", record.prKey).get().get()
            for (d in dupes.documents) {
                if (d.id != record.sessionId) d.reference.update("pr_key", "").get()
            }
        }
        col().document(record.sessionId).set(recordToMap(record)).get()
    }

    override suspend fun get(sessionId: String): ParkRecord? = withContext(Dispatchers.IO) {
        val snap = col().document(sessionId).get().get()
        if (snap.exists()) snapToRecord(snap) else null
    }

    override suspend fun resolveByPrKey(prKey: String): ParkRecord? = withContext(Dispatchers.IO) {
        if (prKey.isEmpty()) return@withContext null // an empty key would match unparked docs (pr_key='')
        db.runTransaction { tx ->
            val snap = tx.get(col().whereEqualTo("pr_key", prKey).limit(1)).get()
            if (snap.isEmpty) return@runTransaction null
            val doc = snap.documents[0]
            if ((doc.getString("pr_key") ?: "").isEmpty()) return@runTransaction null // already claimed
            tx.update(doc.reference, "pr_key", "")
            snapToRecord(doc).copy(prKey = prKey) // hand the claimed key back so the caller stops its timer
        }.get()
    }

    override suspend fun delete(sessionId: String): Unit = withContext(Dispatchers.IO) {
        col().document(sessionId).delete().get()
        Unit
    }

    override suspend fun sweep(cutoff: Instant): List<ParkRecord> = withContext(Dispatchers.IO) {
        // Collect candidates (parked + stale) from a single scan, then claim each in its own
        // transaction so a concurrent resolve cannot double-claim. parked_at is filtered in code to
        // avoid a composite index on (pr_key, parked_at).
        val cutoffMs = cutoff.toEpochMilli()
        val snap = col().whereNotEqualTo("pr_key", "").get().get()
        val candidates = snap.documents.mapNotNull { d ->
            val parkedAt = d.getLong("parked_at")
            val sid = d.getString("session_id")
            val prKey = d.getString("pr_key")
            if (parkedAt != null && parkedAt < cutoffMs && sid != null && !prKey.isNullOrEmpty()) Triple(sid, prKey, Unit) else null
        }
        val out = mutableListOf<ParkRecord>()
        val errors = mutableListOf<Throwable>()
        for ((sid, prKey) in candidates) {
            // A per-doc error skips that candidate (it stays parked for the next sweep) rather than
            // discarding the records already claimed this pass.
            try {
                claimStaleBySession(sid, prKey, cutoffMs)?.let { out += it }
            } catch (e: CancellationException) {
                throw e // never swallow coroutine cancellation
            } catch (e: Exception) {
                errors += e
            }
        }
        // Return everything claimed this pass even if some candidates failed: a claimed record's
        // pr_key is already cleared, so dropping it here would strand it. Throw only when nothing was
        // claimed, so the handler 500s and Cloud Scheduler retries.
        if (out.isEmpty() && errors.isNotEmpty()) throw errors.first()
        out
    }

    // The sweep's per-doc atomic claim, keyed by session id. Inside the transaction it re-checks the
    // doc still carries the expected (stale) pr_key and is still older than the cutoff, so a
    // resolve+re-park between the scan and the claim leaves the fresh park untouched.
    private fun claimStaleBySession(sid: String, prKey: String, cutoffMs: Long): ParkRecord? =
        db.runTransaction { tx ->
            val ref = col().document(sid)
            val snap = tx.get(ref).get()
            if (!snap.exists()) return@runTransaction null
            val curKey = snap.getString("pr_key") ?: ""
            val parkedAt = snap.getLong("parked_at")
            if (curKey != prKey || parkedAt == null || parkedAt >= cutoffMs) return@runTransaction null
            tx.update(ref, "pr_key", "")
            snapToRecord(snap).copy(prKey = prKey)
        }.get()

    override suspend fun parkedCount(): Int = withContext(Dispatchers.IO) {
        col().whereNotEqualTo("pr_key", "").count().get().get().count.toInt()
    }

    override fun close() {
        db.close()
    }

    private fun recordToMap(r: ParkRecord): Map<String, Any?> =
        mapOf(
            "session_id" to r.sessionId,
            "pr_key" to r.prKey,
            "call_id" to r.callId,
            "attempts" to r.attempts.toLong(),
            "params" to r.params,
            "parked_at" to r.parkedAt?.toEpochMilli(),
        )

    private fun snapToRecord(s: DocumentSnapshot): ParkRecord =
        ParkRecord(
            sessionId = s.getString("session_id") ?: "",
            prKey = s.getString("pr_key") ?: "",
            callId = s.getString("call_id") ?: "",
            attempts = (s.getLong("attempts") ?: 0L).toInt(),
            params = s.getString("params") ?: "",
            parkedAt = s.getLong("parked_at")?.let { Instant.ofEpochMilli(it) },
        )
}
