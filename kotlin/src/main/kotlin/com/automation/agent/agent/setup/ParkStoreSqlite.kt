/*
 * The sqlite park store: the file-backed [ParkStore] for SESSION_BACKEND=sqlite, so a parked run
 * survives a restart. It shares its database file with the sqlite session service.
 *
 * adk-kotlin ships no database session service (unlike adk-go/adk-js), so this is hand-rolled on
 * raw JDBC. SQLite serializes writers, so a single pooled connection guarded by a mutex makes the
 * claim CAS and put/sweep contention-free within the process; WAL lets the session service read the
 * shared file without blocking, and busy_timeout makes any cross-connection write wait rather than
 * fail with SQLITE_BUSY. The `pr_key` column doubles as the resume index ('' when not parked), so
 * re-parking under a new key cannot leak a stale index entry.
 */
package com.automation.agent.agent.setup

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import java.sql.Connection
import java.sql.DriverManager
import java.time.Instant

/** A durable park store backed by a local SQLite file via raw JDBC. */
class SqliteParkStore(dsn: String) : ParkStore {
    private val mutex = Mutex()
    private val conn: Connection = DriverManager.getConnection("jdbc:sqlite:$dsn").apply {
        autoCommit = true
        createStatement().use { st ->
            st.execute("PRAGMA journal_mode=WAL")
            st.execute("PRAGMA busy_timeout=5000")
            st.execute(
                """
                CREATE TABLE IF NOT EXISTS parked_runs (
                    session_id TEXT PRIMARY KEY,
                    pr_key     TEXT NOT NULL DEFAULT '',
                    call_id    TEXT NOT NULL DEFAULT '',
                    attempts   INTEGER NOT NULL DEFAULT 0,
                    params     TEXT NOT NULL DEFAULT '',
                    parked_at  INTEGER
                )
                """.trimIndent(),
            )
            st.execute("CREATE INDEX IF NOT EXISTS idx_parked_runs_pr_key ON parked_runs(pr_key)")
        }
    }

    private suspend fun <T> tx(block: (Connection) -> T): T =
        mutex.withLock { withContext(Dispatchers.IO) { block(conn) } }

    // Like [tx], but runs the block in a real transaction so a multi-statement write either lands
    // whole or not at all (no partial park-record state on a mid-op failure). The shared connection
    // is mutex-serialized, so toggling autoCommit here cannot race another caller.
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

    override suspend fun put(record: ParkRecord) = txn { c ->
        if (record.prKey.isNotEmpty()) {
            // One active row per pr_key: clear it on any OTHER session still holding it, so
            // resolve/sweep have a single winner.
            c.prepareStatement("UPDATE parked_runs SET pr_key = '' WHERE pr_key = ? AND session_id <> ?").use { ps ->
                ps.setString(1, record.prKey)
                ps.setString(2, record.sessionId)
                ps.executeUpdate()
            }
        }
        // Upsert by primary key (session id): rewrites every column so the pr_key index follows.
        c.prepareStatement(
            """
            INSERT INTO parked_runs (session_id, pr_key, call_id, attempts, params, parked_at)
            VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT(session_id) DO UPDATE SET
                pr_key = excluded.pr_key, call_id = excluded.call_id, attempts = excluded.attempts,
                params = excluded.params, parked_at = excluded.parked_at
            """.trimIndent(),
        ).use { ps ->
            ps.setString(1, record.sessionId)
            ps.setString(2, record.prKey)
            ps.setString(3, record.callId)
            ps.setInt(4, record.attempts)
            ps.setString(5, record.params)
            setNullableMillis(ps, 6, record.parkedAt)
            ps.executeUpdate()
        }
        Unit
    }

    override suspend fun get(sessionId: String): ParkRecord? = tx { c ->
        c.prepareStatement("SELECT session_id, pr_key, call_id, attempts, params, parked_at FROM parked_runs WHERE session_id = ?").use { ps ->
            ps.setString(1, sessionId)
            ps.executeQuery().use { rs -> if (rs.next()) rowToRecord(rs) else null }
        }
    }

    override suspend fun resolveByPrKey(prKey: String): ParkRecord? = txn { c ->
        if (prKey.isEmpty()) return@txn null // an empty key would match unparked rows (pr_key='')
        val row = c.prepareStatement("SELECT session_id, pr_key, call_id, attempts, params, parked_at FROM parked_runs WHERE pr_key = ? LIMIT 1").use { ps ->
            ps.setString(1, prKey)
            ps.executeQuery().use { rs -> if (rs.next()) rowToRecord(rs) else null }
        } ?: return@txn null
        // CAS: clear pr_key only while it is still set, so of N concurrent claimers exactly one
        // gets a row updated; the rest see 0 and no-op. The row is retained so a retry reads params.
        val claimed = c.prepareStatement("UPDATE parked_runs SET pr_key = '' WHERE session_id = ? AND pr_key = ?").use { ps ->
            ps.setString(1, row.sessionId)
            ps.setString(2, prKey)
            ps.executeUpdate()
        }
        if (claimed == 1) row.copy(prKey = prKey) else null
    }

    override suspend fun delete(sessionId: String) = tx { c ->
        c.prepareStatement("DELETE FROM parked_runs WHERE session_id = ?").use { ps ->
            ps.setString(1, sessionId)
            ps.executeUpdate()
        }
        Unit
    }

    override suspend fun sweep(cutoff: Instant): List<ParkRecord> = txn { c ->
        val cutoffMs = cutoff.toEpochMilli()
        val candidates = c.prepareStatement("SELECT session_id, pr_key, call_id, attempts, params, parked_at FROM parked_runs WHERE pr_key <> '' AND parked_at < ?").use { ps ->
            ps.setLong(1, cutoffMs)
            ps.executeQuery().use { rs -> buildList { while (rs.next()) add(rowToRecord(rs)) } }
        }
        buildList {
            for (row in candidates) {
                // claimStale CAS: also require parked_at < cutoff so a row resolved + re-parked
                // (fresh) after the scan is left alone rather than cleared with a false timeout.
                val claimed = c.prepareStatement("UPDATE parked_runs SET pr_key = '' WHERE session_id = ? AND pr_key = ? AND parked_at < ?").use { ps ->
                    ps.setString(1, row.sessionId)
                    ps.setString(2, row.prKey)
                    ps.setLong(3, cutoffMs)
                    ps.executeUpdate()
                }
                if (claimed == 1) add(row) // row.prKey kept for the caller (timeout sweep needs the PR)
            }
        }
    }

    override suspend fun parkedCount(): Int = tx { c ->
        c.prepareStatement("SELECT COUNT(*) FROM parked_runs WHERE pr_key <> ''").use { ps ->
            ps.executeQuery().use { rs -> if (rs.next()) rs.getInt(1) else 0 }
        }
    }

    override fun close() {
        conn.close()
    }

    private fun rowToRecord(rs: java.sql.ResultSet): ParkRecord {
        val parkedMs = rs.getLong("parked_at")
        val parkedAt = if (rs.wasNull()) null else Instant.ofEpochMilli(parkedMs)
        return ParkRecord(
            sessionId = rs.getString("session_id"),
            prKey = rs.getString("pr_key"),
            callId = rs.getString("call_id"),
            attempts = rs.getInt("attempts"),
            params = rs.getString("params"),
            parkedAt = parkedAt,
        )
    }

    private fun setNullableMillis(ps: java.sql.PreparedStatement, idx: Int, instant: Instant?) {
        if (instant == null) ps.setNull(idx, java.sql.Types.INTEGER) else ps.setLong(idx, instant.toEpochMilli())
    }
}
