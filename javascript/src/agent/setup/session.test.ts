// Tests for the session-service factory. memory and sqlite are wired here; the firestore
// backend is covered by its own (gated) suite. Only the factory mapping is asserted — the
// sqlite service is constructed (its ORM connection opens lazily, so no I/O happens here).
import { DatabaseSessionService, InMemorySessionService } from '@google/adk';
import { describe, expect, it } from 'vitest';

import { newSessionService } from './session';
import { FirestoreSessionService } from './session_firestore';
import { SessionBackend } from '../../config/config';

type Cfg = Parameters<typeof newSessionService>[0];

describe('newSessionService', () => {
  it('builds an in-memory session service for the memory backend', () => {
    const svc = newSessionService({ sessionBackend: SessionBackend.Memory } as Cfg);
    expect(svc).toBeInstanceOf(InMemorySessionService);
  });

  it('builds a database session service for the sqlite backend', () => {
    const svc = newSessionService({
      sessionBackend: SessionBackend.Sqlite,
      sqliteDsn: 'unused.db',
    } as Cfg);
    expect(svc).toBeInstanceOf(DatabaseSessionService);
  });

  it('builds a firestore session service for the firestore backend', () => {
    const svc = newSessionService({
      sessionBackend: SessionBackend.Firestore,
      firestoreProject: 'test-proj',
      firestoreCollection: 'automation_agent',
    } as Cfg);
    expect(svc).toBeInstanceOf(FirestoreSessionService);
  });
});
