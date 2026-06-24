/*
 * The sqlite session service: durable ADK session history for SESSION_BACKEND=sqlite, so a parked
 * fix run can resume after a process restart. It shares its database file with the sqlite park store.
 *
 * adk-kotlin ships no database session service (unlike adk-go's session/database and adk-js's
 * DatabaseSessionService), so this is hand-rolled on raw JDBC. It mirrors InMemorySessionService's
 * observable behavior closely enough for the long-running fix loop: sessions and their events are
 * persisted, and a getSession after a restart rebuilds the session with its event history so the
 * resumable runner can continue.
 *
 * Event (de)serialization reuses the SDK's own configured `Json` (the internal `adkJson`, reached
 * reflectively because adk-kotlin marks it `internal`): an ADK `Event` has `@Serializable` content
 * with `Any`-typed payloads whose contextual serializers are registered only on that instance, so
 * a stock `Json` cannot round-trip it. State scopes: the fix loop never writes app:/user: state, so
 * session state is persisted as a JSON map with `temp:`-prefixed (request-scoped) keys dropped.
 */
package io.github.jkjamies.automationagent.agent.setup

import com.google.adk.kt.events.Event
import com.google.adk.kt.sessions.GetSessionConfig
import com.google.adk.kt.sessions.ListEventsResponse
import com.google.adk.kt.sessions.ListSessionsResponse
import com.google.adk.kt.sessions.Session
import com.google.adk.kt.sessions.SessionKey
import com.google.adk.kt.sessions.SessionService
import com.google.adk.kt.sessions.State
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.doubleOrNull
import kotlinx.serialization.json.longOrNull
import java.sql.Connection
import java.sql.DriverManager
import java.util.UUID

private const val TEMP_PREFIX = "temp:" // request-scoped state keys, never persisted

/** A durable ADK [SessionService] backed by a local SQLite file via raw JDBC. */
class SqliteSessionService(dsn: String) : SessionService {
    private val mutex = Mutex()
    private val conn: Connection = DriverManager.getConnection("jdbc:sqlite:$dsn").apply {
        autoCommit = true
        createStatement().use { st ->
            st.execute("PRAGMA journal_mode=WAL")
            st.execute("PRAGMA busy_timeout=5000")
            st.execute(
                """
                CREATE TABLE IF NOT EXISTS sessions (
                    app_name    TEXT NOT NULL,
                    user_id     TEXT NOT NULL,
                    id          TEXT NOT NULL,
                    state       TEXT NOT NULL DEFAULT '{}',
                    last_update INTEGER NOT NULL,
                    PRIMARY KEY (app_name, user_id, id)
                )
                """.trimIndent(),
            )
            st.execute(
                """
                CREATE TABLE IF NOT EXISTS session_events (
                    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
                    app_name   TEXT NOT NULL,
                    user_id    TEXT NOT NULL,
                    session_id TEXT NOT NULL,
                    timestamp  INTEGER NOT NULL,
                    data       TEXT NOT NULL
                )
                """.trimIndent(),
            )
            st.execute("CREATE INDEX IF NOT EXISTS idx_session_events ON session_events(app_name, user_id, session_id, seq)")
        }
    }

    private suspend fun <T> tx(block: (Connection) -> T): T =
        mutex.withLock { withContext(Dispatchers.IO) { block(conn) } }

    // Like [tx], but runs the block in a real transaction so a write that touches both the sessions
    // and session_events tables either lands whole or not at all (they cannot desync on a mid-op
    // failure). The shared connection is mutex-serialized, so toggling autoCommit cannot race.
    private suspend fun <T> txn(block: (Connection) -> T): T =
        mutex.withLock {
            withContext(Dispatchers.IO) {
                conn.autoCommit = false
                try {
                    val result = block(conn)
                    conn.commit()
                    result
                } catch (e: Throwable) {
                    runCatching { conn.rollback() }
                    throw e
                } finally {
                    conn.autoCommit = true
                }
            }
        }

    override suspend fun createSession(key: SessionKey, state: Map<String, Any>?): Session {
        val id = key.id?.ifEmpty { null } ?: UUID.randomUUID().toString()
        val resolved = SessionKey(key.appName, key.userId, id)
        val initial: Map<String, Any> = state ?: emptyMap()
        val persistable = initial.filterKeys { !it.startsWith(TEMP_PREFIX) }
        val now = System.currentTimeMillis()
        txn { c ->
            // A fresh session: replace any stale rows under this key.
            c.prepareStatement("DELETE FROM session_events WHERE app_name = ? AND user_id = ? AND session_id = ?").use { ps ->
                ps.setString(1, resolved.appName); ps.setString(2, resolved.userId); ps.setString(3, id)
                ps.executeUpdate()
            }
            c.prepareStatement(
                "INSERT INTO sessions (app_name, user_id, id, state, last_update) VALUES (?, ?, ?, ?, ?) " +
                    "ON CONFLICT(app_name, user_id, id) DO UPDATE SET state = excluded.state, last_update = excluded.last_update",
            ).use { ps ->
                ps.setString(1, resolved.appName); ps.setString(2, resolved.userId); ps.setString(3, id)
                ps.setString(4, encodeState(persistable)); ps.setLong(5, now)
                ps.executeUpdate()
            }
        }
        return Session(resolved, State(initial.toMutableMap(), mutableMapOf()), mutableListOf(), kotlin.time.Instant.fromEpochMilliseconds(now))
    }

    override suspend fun getSession(key: SessionKey, config: GetSessionConfig?): Session? = tx { c ->
        val row = c.prepareStatement("SELECT state, last_update FROM sessions WHERE app_name = ? AND user_id = ? AND id = ?").use { ps ->
            ps.setString(1, key.appName); ps.setString(2, key.userId); ps.setString(3, key.id)
            ps.executeQuery().use { rs -> if (rs.next()) Pair(rs.getString("state"), rs.getLong("last_update")) else null }
        } ?: return@tx null

        val events = loadEvents(c, key)
        val afterMs = config?.afterTimestamp?.toEpochMilliseconds()
        val filtered = events
            .let { evs -> if (afterMs != null) evs.filter { it.timestamp >= afterMs } else evs }
            .let { evs -> config?.numRecentEvents?.let { n -> evs.takeLast(n) } ?: evs }
        Session(key, State(decodeState(row.first).toMutableMap(), mutableMapOf()), filtered.toMutableList(), kotlin.time.Instant.fromEpochMilliseconds(row.second))
    }

    override suspend fun listSessions(appName: String, userId: String): ListSessionsResponse = tx { c ->
        // Sessions without their events (the ADK convention for a list view).
        val sessions = c.prepareStatement("SELECT id, state, last_update FROM sessions WHERE app_name = ? AND user_id = ?").use { ps ->
            ps.setString(1, appName); ps.setString(2, userId)
            ps.executeQuery().use { rs ->
                buildList {
                    while (rs.next()) {
                        add(
                            Session(
                                SessionKey(appName, userId, rs.getString("id")),
                                State(decodeState(rs.getString("state")).toMutableMap(), mutableMapOf()),
                                mutableListOf(),
                                kotlin.time.Instant.fromEpochMilliseconds(rs.getLong("last_update")),
                            ),
                        )
                    }
                }
            }
        }
        ListSessionsResponse(sessions)
    }

    override suspend fun deleteSession(key: SessionKey): Unit = txn { c ->
        c.prepareStatement("DELETE FROM session_events WHERE app_name = ? AND user_id = ? AND session_id = ?").use { ps ->
            ps.setString(1, key.appName); ps.setString(2, key.userId); ps.setString(3, key.id)
            ps.executeUpdate()
        }
        c.prepareStatement("DELETE FROM sessions WHERE app_name = ? AND user_id = ? AND id = ?").use { ps ->
            ps.setString(1, key.appName); ps.setString(2, key.userId); ps.setString(3, key.id)
            ps.executeUpdate()
        }
        Unit
    }

    override suspend fun listEvents(key: SessionKey): ListEventsResponse = tx { c ->
        ListEventsResponse(loadEvents(c, key), "")
    }

    override suspend fun appendEvent(session: Session, event: Event): Event {
        val key = session.key
        // Apply the event's state delta to the live session (the runner reads it back), then persist
        // the event and the updated, durable (non-temp) state under one lock.
        session.state.applyDelta(event.actions.stateDelta)
        val persistable = session.state.filterKeys { !it.startsWith(TEMP_PREFIX) }
        val now = System.currentTimeMillis()
        txn { c ->
            c.prepareStatement("INSERT INTO session_events (app_name, user_id, session_id, timestamp, data) VALUES (?, ?, ?, ?, ?)").use { ps ->
                ps.setString(1, key.appName); ps.setString(2, key.userId); ps.setString(3, key.id)
                ps.setLong(4, event.timestamp); ps.setString(5, adkEventJson.encodeToString(Event.serializer(), event))
                ps.executeUpdate()
            }
            c.prepareStatement("UPDATE sessions SET state = ?, last_update = ? WHERE app_name = ? AND user_id = ? AND id = ?").use { ps ->
                ps.setString(1, encodeState(persistable)); ps.setLong(2, now)
                ps.setString(3, key.appName); ps.setString(4, key.userId); ps.setString(5, key.id)
                ps.executeUpdate()
            }
        }
        // Mirror InMemorySessionService: reflect the event into the live session's history.
        @Suppress("UNCHECKED_CAST")
        (session.events as? MutableList<Event>)?.add(event)
        session.lastUpdateTime = kotlin.time.Instant.fromEpochMilliseconds(now)
        return event
    }

    override suspend fun closeSession(session: Session) {
        // No per-session resources; the shared connection is released on shutdown.
    }

    private fun loadEvents(c: Connection, key: SessionKey): List<Event> =
        c.prepareStatement("SELECT data FROM session_events WHERE app_name = ? AND user_id = ? AND session_id = ? ORDER BY seq ASC").use { ps ->
            ps.setString(1, key.appName); ps.setString(2, key.userId); ps.setString(3, key.id)
            ps.executeQuery().use { rs ->
                buildList { while (rs.next()) add(adkEventJson.decodeFromString(Event.serializer(), rs.getString("data"))) }
            }
        }

    private fun encodeState(state: Map<String, Any>): String =
        JsonObject(state.mapValues { anyToJson(it.value) }).toString()

    private fun decodeState(json: String): Map<String, Any> {
        if (json.isBlank()) return emptyMap()
        val obj = Json.parseToJsonElement(json) as? JsonObject ?: return emptyMap()
        return obj.entries.mapNotNull { (k, v) -> jsonToAny(v)?.let { k to it } }.toMap()
    }
}

// State values originate from JSON tool output (primitives, maps, lists), so a JsonElement bridge
// covers them without the SDK's internal Any serializer.
private fun anyToJson(v: Any?): JsonElement = when (v) {
    null -> JsonNull
    is JsonElement -> v
    is String -> JsonPrimitive(v)
    is Boolean -> JsonPrimitive(v)
    is Number -> JsonPrimitive(v)
    is Map<*, *> -> JsonObject(v.entries.associate { (k, x) -> k.toString() to anyToJson(x) })
    is Iterable<*> -> JsonArray(v.map { anyToJson(it) })
    else -> JsonPrimitive(v.toString())
}

private fun jsonToAny(e: JsonElement): Any? = when (e) {
    is JsonNull -> null
    is JsonPrimitive ->
        if (e.isString) e.content
        else e.booleanOrNull ?: e.longOrNull ?: e.doubleOrNull ?: e.content
    // Preserve nulls inside nested objects/arrays so a value's shape round-trips unchanged.
    is JsonObject -> e.entries.associate { (k, v) -> k to jsonToAny(v) }
    is JsonArray -> e.map { jsonToAny(it) }
}
