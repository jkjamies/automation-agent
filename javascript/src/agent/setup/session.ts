/**
 * The session-service factory: selects where the suspend/resume session history lives.
 *
 * The fix loop suspends on a long-running tool and resumes when CI reports; the session
 * service is what holds that conversation across the wait. `memory` (the default) keeps
 * it in-process — a restart drops parked runs; the durable backends persist it so a
 * parked run survives a restart. Kept in `setup` because it touches the ADK session
 * SDK (confined here by the arch tests).
 */
import { resolve } from 'node:path';

import { type BaseSessionService, DatabaseSessionService, InMemorySessionService } from '@google/adk';

import { type Config, SessionBackend } from '../../config/config';
import { FirestoreSessionService } from './session_firestore';

/** Build the ADK session service for the configured backend. */
export function newSessionService(cfg: Config): BaseSessionService {
  switch (cfg.sessionBackend) {
    case SessionBackend.Memory:
      return new InMemorySessionService();
    case SessionBackend.Sqlite:
      // ADK's database-backed session service over MikroORM; the `sqlite://` URI takes an
      // absolute path so it shares the configured file with the sqlite park store.
      return new DatabaseSessionService(`sqlite://${resolve(cfg.sqliteDsn)}`);
    case SessionBackend.Firestore:
      return new FirestoreSessionService(cfg.firestoreProject, cfg.firestoreCollection);
  }
}
