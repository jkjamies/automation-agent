// Emulator-gated conformance for the firestore park store. These tests run only when a
// Cloud Firestore emulator is configured via FIRESTORE_EMULATOR_HOST (so they are skipped
// in the default `make ci`/coverage gate); run them with `make cover-firestore`. They
// assert the same single-winner claim / sweep semantics the in-memory and sqlite backends
// satisfy, against a real Firestore transaction.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { FirestoreParkStore } from './parkstore_firestore';
import { type ParkRecord } from './parkstore';

const EMULATOR = process.env.FIRESTORE_EMULATOR_HOST;
const PROJECT = process.env.FIRESTORE_PROJECT ?? 'automation-agent-test';

function record(overrides: Partial<ParkRecord> = {}): ParkRecord {
  return {
    sessionId: 's1',
    prKey: 'acme/api#1',
    callId: 'c1',
    attempts: 1,
    params: '{}',
    parkedAt: new Date(),
    ...overrides,
  };
}

// A unique collection per test keeps runs isolated without an explicit wipe.
let n = 0;

describe.skipIf(!EMULATOR)('FirestoreParkStore (emulator)', () => {
  let store: FirestoreParkStore;

  beforeEach(() => {
    n += 1;
    store = new FirestoreParkStore(PROJECT, `parked_runs_test_${process.pid}_${n}`);
  });
  afterEach(async () => {
    await store.close();
  });

  it('puts, gets, and counts parked records', async () => {
    await store.put(record({ sessionId: 'unparked', prKey: '' }));
    expect(await store.parkedCount()).toBe(0);
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#1', params: '{"x":1}' }));
    expect((await store.get('s1'))?.params).toBe('{"x":1}');
    expect(await store.parkedCount()).toBe(1);
    expect(await store.get('missing')).toBeNull();
  });

  it('resolves a PR key exactly once and retains the record', async () => {
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#1', params: '{"x":1}' }));
    const claimed = await store.resolveByPrKey('acme/api#1');
    expect(claimed?.sessionId).toBe('s1');
    expect(claimed?.prKey).toBe('acme/api#1');
    expect(await store.resolveByPrKey('acme/api#1')).toBeNull();
    expect(await store.parkedCount()).toBe(0);
    expect((await store.get('s1'))?.params).toBe('{"x":1}'); // retained for retry
  });

  it('returns null for an empty or unknown PR key', async () => {
    expect(await store.resolveByPrKey('')).toBeNull();
    expect(await store.resolveByPrKey('never/parked#9')).toBeNull();
  });

  it('drops the stale index when a session re-parks under a new key', async () => {
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#1' }));
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#2' }));
    expect(await store.parkedCount()).toBe(1);
    expect(await store.resolveByPrKey('acme/api#1')).toBeNull();
    expect((await store.resolveByPrKey('acme/api#2'))?.sessionId).toBe('s1');
  });

  it('sweeps only records parked before the cutoff, claiming each once', async () => {
    await store.put(record({ sessionId: 'old', prKey: 'acme/api#1', parkedAt: new Date(Date.now() - 10_000) }));
    await store.put(record({ sessionId: 'fresh', prKey: 'acme/api#2', parkedAt: new Date() }));
    const cutoff = new Date(Date.now() - 5_000);
    const claimed = await store.sweep(cutoff);
    expect(claimed.map((r) => r.sessionId)).toEqual(['old']);
    expect(claimed[0]!.prKey).toBe('acme/api#1');
    expect(await store.sweep(cutoff)).toHaveLength(0);
    expect(await store.parkedCount()).toBe(1);
  });

  it('deletes a record and clears its index', async () => {
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#1' }));
    await store.delete('s1');
    expect(await store.get('s1')).toBeNull();
    expect(await store.parkedCount()).toBe(0);
  });
});
