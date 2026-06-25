/*
 * The firestore session service: durable ADK session history for SESSION_BACKEND=firestore — the
 * serverless, scale-to-zero cloud backend. Hand-rolled like Go's (adk-kotlin ships no cloud session
 * service). Sessions are documents carrying their session-scoped state; events live in an "events"
 * sub-collection (not an array field) so a long-lived session cannot exceed Firestore's 1 MiB
 * per-document limit. ADK Events are (de)serialized with the SDK's own [adkEventJson]; state is
 * stored as native Firestore map values. temp:-prefixed (request-scoped) state keys are dropped.
 *
 * Exercised only under the firestore emulator (emulator-gated tests) and excluded from the coverage
 * floor.
 */
package com.automation.agent.agent.setup

import com.google.adk.kt.events.Event
import com.google.adk.kt.sessions.GetSessionConfig
import com.google.adk.kt.sessions.ListEventsResponse
import com.google.adk.kt.sessions.ListSessionsResponse
import com.google.adk.kt.sessions.Session
import com.google.adk.kt.sessions.SessionKey
import com.google.adk.kt.sessions.SessionService
import com.google.adk.kt.sessions.State
import com.google.cloud.firestore.DocumentReference
import com.google.cloud.firestore.Firestore
import com.google.cloud.firestore.FirestoreOptions
import com.google.cloud.firestore.Query
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.util.Base64
import java.util.UUID

private const val FS_TEMP_PREFIX = "temp:" // request-scoped state keys, never persisted

/**
 * A Firestore-safe document id from a key's parts (base64url of a unit-separator-joined key), so
 * arbitrary app/user/session ids cannot collide or contain illegal characters. Mirrors Go.
 */
private fun encodeKey(vararg parts: String): String =
    Base64.getUrlEncoder().withoutPadding().encodeToString(parts.joinToString("").toByteArray())

/** A durable ADK [SessionService] backed by Cloud Firestore. */
class FirestoreSessionService(project: String, private val collection: String) : SessionService {
    private val db: Firestore =
        FirestoreOptions.newBuilder().apply { if (project.isNotEmpty()) setProjectId(project) }.build().service

    private fun docId(key: SessionKey, id: String = key.id ?: "") = encodeKey(key.appName, key.userId, id)

    private fun ref(key: SessionKey, id: String = key.id ?: ""): DocumentReference =
        db.collection(collection).document(docId(key, id))

    override suspend fun createSession(key: SessionKey, state: Map<String, Any>?): Session =
        withContext(Dispatchers.IO) {
            val id = key.id?.ifEmpty { null } ?: UUID.randomUUID().toString()
            val resolved = SessionKey(key.appName, key.userId, id)
            val initial: Map<String, Any> = state ?: emptyMap()
            val persistable = initial.filterKeys { !it.startsWith(FS_TEMP_PREFIX) }
            val now = System.currentTimeMillis()
            val sessionRef = ref(resolved, id)
            // A fresh session: purge any stale events left under this id (Firestore does not cascade,
            // and the doc set below only resets next_seq=0). The purge and the reset commit together
            // in one batch, so a reused session id can never be left with partial history. Mirrors
            // the sqlite backend's reset.
            val batch = db.batch()
            for (ev in sessionRef.collection("events").get().get().documents) batch.delete(ev.reference)
            batch.set(
                sessionRef,
                mapOf(
                    "app_name" to resolved.appName, "user_id" to resolved.userId, "session_id" to id,
                    "state" to persistable, "next_seq" to 0L, "updated_at" to now,
                ),
            )
            batch.commit().get()
            Session(resolved, State(initial.toMutableMap(), mutableMapOf()), mutableListOf(), kotlin.time.Instant.fromEpochMilliseconds(now))
        }

    override suspend fun getSession(key: SessionKey, config: GetSessionConfig?): Session? =
        withContext(Dispatchers.IO) {
            val snap = ref(key).get().get()
            if (!snap.exists()) return@withContext null
            val updated = snap.getLong("updated_at") ?: 0L
            val events = loadEvents(ref(key))
            val afterMs = config?.afterTimestamp?.toEpochMilliseconds()
            val filtered = events
                .let { evs -> if (afterMs != null) evs.filter { it.timestamp >= afterMs } else evs }
                .let { evs -> config?.numRecentEvents?.let { n -> evs.takeLast(n) } ?: evs }
            Session(key, State(readState(snap.get("state")), mutableMapOf()), filtered.toMutableList(), kotlin.time.Instant.fromEpochMilliseconds(updated))
        }

    override suspend fun listSessions(appName: String, userId: String): ListSessionsResponse =
        withContext(Dispatchers.IO) {
            val docs = db.collection(collection).whereEqualTo("app_name", appName).whereEqualTo("user_id", userId).get().get()
            // Sessions without their events (the ADK convention for a list view).
            val sessions = docs.documents.map { snap ->
                Session(
                    SessionKey(appName, userId, snap.getString("session_id") ?: ""),
                    State(readState(snap.get("state")), mutableMapOf()),
                    mutableListOf(),
                    kotlin.time.Instant.fromEpochMilliseconds(snap.getLong("updated_at") ?: 0L),
                )
            }
            ListSessionsResponse(sessions)
        }

    override suspend fun deleteSession(key: SessionKey): Unit = withContext(Dispatchers.IO) {
        // Firestore does not cascade: delete the events sub-collection before the session doc.
        val sessionRef = ref(key)
        for (ev in sessionRef.collection("events").get().get().documents) ev.reference.delete().get()
        sessionRef.delete().get()
        Unit
    }

    override suspend fun listEvents(key: SessionKey): ListEventsResponse =
        withContext(Dispatchers.IO) { ListEventsResponse(loadEvents(ref(key)), "") }

    override suspend fun appendEvent(session: Session, event: Event): Event {
        val key = session.key
        session.state.applyDelta(event.actions.stateDelta)
        val persistable = session.state.filterKeys { !it.startsWith(FS_TEMP_PREFIX) }
        val now = System.currentTimeMillis()
        val blob = adkEventJson.encodeToString(Event.serializer(), event)
        withContext(Dispatchers.IO) {
            val sessionRef = ref(key)
            // One transaction advances the session (state + next_seq) and writes the event together,
            // so the two cannot desync. All reads precede all writes, as Firestore requires.
            db.runTransaction { tx ->
                val snap = tx.get(sessionRef).get()
                if (!snap.exists()) throw IllegalStateException("session not found, cannot apply event")
                val seq = snap.getLong("next_seq") ?: 0L
                tx.set(
                    sessionRef,
                    mapOf(
                        "app_name" to key.appName, "user_id" to key.userId, "session_id" to key.id,
                        "state" to persistable, "next_seq" to seq + 1, "updated_at" to now,
                    ),
                )
                tx.set(
                    sessionRef.collection("events").document(seq.toString().padStart(20, '0')),
                    mapOf("seq" to seq, "timestamp" to event.timestamp, "blob" to blob),
                )
                seq
            }.get()
        }
        // Mirror InMemorySessionService: reflect the event into the live session's history.
        @Suppress("UNCHECKED_CAST")
        (session.events as? MutableList<Event>)?.add(event)
        session.lastUpdateTime = kotlin.time.Instant.fromEpochMilliseconds(now)
        return event
    }

    override suspend fun closeSession(session: Session) {
        // No per-session resources; the Firestore client is process-scoped.
    }

    private fun loadEvents(sessionRef: DocumentReference): List<Event> =
        sessionRef.collection("events").orderBy("seq", Query.Direction.ASCENDING).get().get()
            .documents.mapNotNull { it.getString("blob")?.let { blob -> adkEventJson.decodeFromString(Event.serializer(), blob) } }

    @Suppress("UNCHECKED_CAST")
    private fun readState(raw: Any?): MutableMap<String, Any> =
        (raw as? Map<String, Any>)?.toMutableMap() ?: mutableMapOf()
}
