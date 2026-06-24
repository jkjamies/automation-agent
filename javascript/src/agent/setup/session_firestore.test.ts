// Emulator-gated tests for the firestore session service. They run only when
// FIRESTORE_EMULATOR_HOST is set (skipped in the default gate); run with `make cover-firestore`.
// They assert the durable semantics the fix loop relies on: state scopes (app:/user:/temp:),
// event persistence in the sub-collection (ordered, filterable), and create/get/delete.
import { type Event, createEvent } from '@google/adk';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { FirestoreSessionService } from './session_firestore';

const EMULATOR = process.env.FIRESTORE_EMULATOR_HOST;
const PROJECT = process.env.FIRESTORE_PROJECT ?? 'automation-agent-test';

// Build an event, setting its state delta on the default (full) EventActions createEvent fills.
function ev(timestamp: number, opts: { stateDelta?: Record<string, unknown>; partial?: boolean } = {}): Event {
  const e = createEvent({ author: 'user', timestamp, partial: opts.partial });
  if (opts.stateDelta) {
    e.actions.stateDelta = opts.stateDelta;
  }
  return e;
}

let n = 0;

describe.skipIf(!EMULATOR)('FirestoreSessionService (emulator)', () => {
  let svc: FirestoreSessionService;
  const app = 'app';
  const user = 'u';

  beforeEach(() => {
    n += 1;
    svc = new FirestoreSessionService(PROJECT, `sess_test_${process.pid}_${n}`);
  });
  afterEach(async () => {
    await svc.close();
  });

  it('creates and reads back a session, splitting state by scope', async () => {
    const created = await svc.createSession({
      appName: app,
      userId: user,
      sessionId: 's1',
      state: { 'app:shared': 1, 'user:pref': 'x', sessionKey: 'v', 'temp:scratch': 'drop' },
    });
    expect(created.id).toBe('s1');
    // Merged view exposes app:/user:/session keys; temp: is dropped.
    expect(created.state['app:shared']).toBe(1);
    expect(created.state['user:pref']).toBe('x');
    expect(created.state.sessionKey).toBe('v');
    expect(created.state['temp:scratch']).toBeUndefined();

    const got = await svc.getSession({ appName: app, userId: user, sessionId: 's1' });
    expect(got?.state['app:shared']).toBe(1);
    expect(got?.state['user:pref']).toBe('x');
    expect(got?.state.sessionKey).toBe('v');
    expect(got?.state['temp:scratch']).toBeUndefined();
  });

  it('rejects a duplicate session id', async () => {
    await svc.createSession({ appName: app, userId: user, sessionId: 'dup' });
    await expect(
      svc.createSession({ appName: app, userId: user, sessionId: 'dup' }),
    ).rejects.toThrow(/already exists/);
  });

  it('persists non-partial events in order and skips partial ones', async () => {
    const session = await svc.createSession({ appName: app, userId: user, sessionId: 's1' });
    await svc.appendEvent({ session, event: ev(1000, { stateDelta: { a: 1 } }) });
    await svc.appendEvent({ session, event: ev(2000, { partial: true }) });
    await svc.appendEvent({ session, event: ev(3000, { stateDelta: { b: 2 } }) });

    const got = await svc.getSession({ appName: app, userId: user, sessionId: 's1' });
    expect(got?.events.map((e) => e.timestamp)).toEqual([1000, 3000]); // partial skipped, in order
    expect(got?.state.a).toBe(1);
    expect(got?.state.b).toBe(2);
    // appendEvent reflected the deltas on the in-memory session too.
    expect(session.state.a).toBe(1);
    expect(session.state.b).toBe(2);
  });

  it('applies the numRecentEvents and afterTimestamp filters', async () => {
    const session = await svc.createSession({ appName: app, userId: user, sessionId: 's1' });
    for (const ts of [1000, 2000, 3000]) {
      await svc.appendEvent({ session, event: ev(ts) });
    }
    const recent = await svc.getSession({
      appName: app,
      userId: user,
      sessionId: 's1',
      config: { numRecentEvents: 2 },
    });
    expect(recent?.events.map((e) => e.timestamp)).toEqual([2000, 3000]);

    const after = await svc.getSession({
      appName: app,
      userId: user,
      sessionId: 's1',
      config: { afterTimestamp: 2500 },
    });
    expect(after?.events.map((e) => e.timestamp)).toEqual([3000]);
  });

  it('returns undefined for a missing session and deletes one with its events', async () => {
    expect(await svc.getSession({ appName: app, userId: user, sessionId: 'nope' })).toBeUndefined();
    const session = await svc.createSession({ appName: app, userId: user, sessionId: 's1' });
    await svc.appendEvent({ session, event: ev(1000) });
    await svc.deleteSession({ appName: app, userId: user, sessionId: 's1' });
    expect(await svc.getSession({ appName: app, userId: user, sessionId: 's1' })).toBeUndefined();
  });

  it('lists sessions for an app without their event history', async () => {
    await svc.createSession({ appName: app, userId: user, sessionId: 's1' });
    await svc.createSession({ appName: app, userId: user, sessionId: 's2' });
    const res = await svc.listSessions({ appName: app, userId: user });
    expect(res.sessions.map((s) => s.id).sort()).toEqual(['s1', 's2']);
    expect(res.totalItems).toBe(2);
    expect(res.sessions.every((s) => s.events.length === 0)).toBe(true);
  });
});
