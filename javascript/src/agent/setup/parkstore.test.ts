// Conformance tests for the park store: single-winner claim, value semantics, stale-index
// hygiene, and the sweep cutoff. The same suite runs against every backend (memory and the
// durable sqlite file) so they stay behaviorally identical; the firestore backend reuses
// these expectations under its own (emulator-gated) suite.
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { MemoryParkStore, type ParkRecord, type ParkStore, isParked, newParkStore } from './parkstore';
import { FirestoreParkStore } from './parkstore_firestore';
import { SqliteParkStore } from './parkstore_sqlite';
import { SessionBackend } from '../../config/config';

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

// Each backend supplies a fresh store per test (and any teardown it needs).
interface Backend {
  name: string;
  setup(): { store: ParkStore; teardown: () => void };
}

const backends: Backend[] = [
  {
    name: 'MemoryParkStore',
    setup: () => ({ store: new MemoryParkStore(), teardown: () => {} }),
  },
  {
    name: 'SqliteParkStore',
    setup: () => {
      const dir = mkdtempSync(join(tmpdir(), 'parkstore-'));
      const store = new SqliteParkStore(join(dir, 'parks.db'));
      return {
        store,
        teardown: () => {
          void store.close();
          rmSync(dir, { recursive: true, force: true });
        },
      };
    },
  },
];

describe.each(backends)('$name (conformance)', ({ setup }) => {
  let store: ParkStore;
  let teardown: () => void;
  beforeEach(() => {
    ({ store, teardown } = setup());
  });
  afterEach(() => {
    teardown();
  });

  it('puts and gets a record by session id', async () => {
    await store.put(record({ sessionId: 's1' }));
    const got = await store.get('s1');
    expect(got?.callId).toBe('c1');
    expect(await store.get('missing')).toBeNull();
  });

  it('returns copies so a caller mutation cannot corrupt the store', async () => {
    await store.put(record({ sessionId: 's1', attempts: 1 }));
    const got = (await store.get('s1'))!;
    got.attempts = 999;
    got.parkedAt = new Date(0);
    const again = (await store.get('s1'))!;
    expect(again.attempts).toBe(1);
    expect(again.parkedAt?.getTime()).not.toBe(0);
  });

  it('counts only parked (PR-key-indexed) records', async () => {
    await store.put(record({ sessionId: 'unparked', prKey: '' }));
    expect(await store.parkedCount()).toBe(0);
    await store.put(record({ sessionId: 'parked', prKey: 'acme/api#7' }));
    expect(await store.parkedCount()).toBe(1);
  });

  it('resolves a PR key exactly once and retains the record for retry', async () => {
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#1', params: '{"x":1}' }));
    const claimed = await store.resolveByPrKey('acme/api#1');
    expect(claimed?.sessionId).toBe('s1');
    expect(claimed?.prKey).toBe('acme/api#1'); // returned copy carries the claimed key
    // A second claim finds nothing (single winner)…
    expect(await store.resolveByPrKey('acme/api#1')).toBeNull();
    expect(await store.parkedCount()).toBe(0);
    // …but the per-run record is retained (unparked) so a retry can read its params.
    const retained = await store.get('s1');
    expect(retained?.params).toBe('{"x":1}');
    expect(isParked(retained!)).toBe(false);
  });

  it('returns null for an empty or unknown PR key', async () => {
    expect(await store.resolveByPrKey('')).toBeNull();
    expect(await store.resolveByPrKey('never/parked#9')).toBeNull();
  });

  it('drops the stale index when a session re-parks under a new key', async () => {
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#1' }));
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#2' }));
    expect(await store.parkedCount()).toBe(1);
    expect(await store.resolveByPrKey('acme/api#1')).toBeNull(); // old key no longer indexed
    expect((await store.resolveByPrKey('acme/api#2'))?.sessionId).toBe('s1');
  });

  it('deletes a record and clears its index', async () => {
    await store.put(record({ sessionId: 's1', prKey: 'acme/api#1' }));
    await store.delete('s1');
    expect(await store.get('s1')).toBeNull();
    expect(await store.parkedCount()).toBe(0);
    await store.delete('s1'); // no-op on absent
  });

  it('sweeps only records parked before the cutoff, claiming each once', async () => {
    const old = new Date(Date.now() - 10_000);
    const fresh = new Date();
    await store.put(record({ sessionId: 'old', prKey: 'acme/api#1', parkedAt: old }));
    await store.put(record({ sessionId: 'fresh', prKey: 'acme/api#2', parkedAt: fresh }));

    const cutoff = new Date(Date.now() - 5_000);
    const claimed = await store.sweep(cutoff);
    expect(claimed.map((r) => r.sessionId)).toEqual(['old']);
    expect(claimed[0]!.prKey).toBe('acme/api#1'); // keeps its key for cleanup
    // The swept run is no longer parked; a re-sweep claims nothing more.
    expect(await store.sweep(cutoff)).toHaveLength(0);
    expect(await store.parkedCount()).toBe(1); // only the fresh run remains parked
  });

  it('close is idempotent-friendly', async () => {
    await expect(store.close()).resolves.toBeUndefined();
  });
});

describe('SqliteParkStore durability', () => {
  it('survives a close and reopen on the same file', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'parkstore-durable-'));
    const dbPath = join(dir, 'parks.db');
    try {
      const first = new SqliteParkStore(dbPath);
      await first.put(
        record({ sessionId: 's1', prKey: 'acme/api#1', params: '{"k":1}', attempts: 2 }),
      );
      await first.close();

      // A fresh store on the same file (a process restart) still sees the parked run.
      const second = new SqliteParkStore(dbPath);
      const got = await second.get('s1');
      expect(got?.attempts).toBe(2);
      expect(got?.params).toBe('{"k":1}');
      const claimed = await second.resolveByPrKey('acme/api#1');
      expect(claimed?.sessionId).toBe('s1');
      await second.close();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('newParkStore', () => {
  it('builds the memory backend', () => {
    const cfg = { sessionBackend: SessionBackend.Memory } as Parameters<typeof newParkStore>[0];
    expect(newParkStore(cfg)).toBeInstanceOf(MemoryParkStore);
  });

  it('builds the sqlite backend', () => {
    const dir = mkdtempSync(join(tmpdir(), 'parkstore-factory-'));
    try {
      const cfg = {
        sessionBackend: SessionBackend.Sqlite,
        sqliteDsn: join(dir, 'f.db'),
      } as Parameters<typeof newParkStore>[0];
      const store = newParkStore(cfg);
      expect(store).toBeInstanceOf(SqliteParkStore);
      void store.close();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('builds the firestore backend', () => {
    // Constructs the client (no connection happens until a query), so this is safe offline.
    const cfg = {
      sessionBackend: SessionBackend.Firestore,
      firestoreProject: 'test-proj',
      firestoreCollection: 'automation_agent',
    } as Parameters<typeof newParkStore>[0];
    const store = newParkStore(cfg);
    expect(store).toBeInstanceOf(FirestoreParkStore);
    void store.close();
  });
});
