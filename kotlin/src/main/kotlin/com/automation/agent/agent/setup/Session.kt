/*
 * The ADK session-service factory: builds the durable suspend/resume session backend for the
 * configured SESSION_BACKEND. The session service holds the history that lets a parked fix run
 * continue after a restart; only the long-running fix loop needs it (ephemeral one-shot runners
 * keep their own in-memory session).
 *
 *   memory    -> in-process (today's behavior, default)
 *   sqlite    -> file-backed via the hand-rolled [SqliteSessionService]; durable local runs
 *   firestore -> cloud, via the hand-rolled Firestore session service; durable cloud runs
 */
package com.automation.agent.agent.setup

import com.google.adk.kt.sessions.InMemorySessionService
import com.google.adk.kt.sessions.SessionService
import com.automation.agent.config.Config
import com.automation.agent.config.SessionBackend
import java.io.File

/** Build the ADK session service for the configured session backend. */
fun newSessionService(cfg: Config): SessionService =
    when (cfg.sessionBackend) {
        SessionBackend.MEMORY -> InMemorySessionService()
        // Absolute path so the session service and the park store open the very same file
        // regardless of the process working directory.
        SessionBackend.SQLITE -> SqliteSessionService(File(cfg.sqliteDsn).absolutePath)
        SessionBackend.FIRESTORE -> FirestoreSessionService(cfg.firestoreProject, "${cfg.firestoreCollection}_sessions")
    }
