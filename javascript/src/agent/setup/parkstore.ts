/**
 * The park store: the durable seam that replaces the in-memory parked-run registry.
 *
 * A {@link ParkRecord} is the per-run state a suspended fix loop needs to resume —
 * which session, which long-running call, how many attempts, and the serialized run
 * params — keyed by a globally unique session id and indexed by PR key
 * (`owner/repo#pr`) while parked. The store provides single-winner claim semantics
 * ({@link ParkStore.resolveByPrKey}, {@link ParkStore.sweep}) so exactly one of
 * {CI webhook, soft timer, sweep} ever resolves a given run.
 *
 * Backends: `memory` (in-process, the default; a restart drops parked runs), `sqlite`,
 * and `firestore`. Only the session service has an ADK-native implementation; the park
 * store is our own concept and is always hand-rolled. This module holds the interface,
 * the memory backend, and the {@link newParkStore} factory; the durable backends live in
 * sibling modules and are wired into the factory.
 */
import { resolve } from 'node:path';

import { type Config, SessionBackend } from '../../config/config';
import { FirestoreParkStore } from './parkstore_firestore';
import { SqliteParkStore } from './parkstore_sqlite';

/**
 * The per-run state of one suspended fix loop. `prKey` is empty until the run parks on
 * CI (it is the resume index, `owner/repo#pr`); `params` is the caller's opaque
 * serialized run inputs; `parkedAt` is when the run parked (the sweep cutoff reads it).
 */
export interface ParkRecord {
  /** Globally unique session id (a UUID); stable from kickoff through every retry. */
  sessionId: string;
  /** Resume index `owner/repo#pr`; empty until the run parks awaiting CI. */
  prKey: string;
  /** The parked long-running call id (ADK), fed back on resume. */
  callId: string;
  /** Attempt count (caller-tracked, not GitHub's). */
  attempts: number;
  /** Opaque caller-serialized run params (see the Driver's RunParams). */
  params: string;
  /** When the run parked (UTC); null until parked. The sweep cutoff compares against it. */
  parkedAt: Date | null;
}

/** Whether a record is currently parked (indexed for resume). */
export function isParked(r: ParkRecord): boolean {
  return r.prKey !== '';
}

/**
 * Persists parked-run state with single-winner claim semantics.
 *
 * Implementations must make {@link resolveByPrKey} and {@link sweep} atomic: for a given
 * PR key (or stale record) exactly one concurrent caller wins the claim and the rest get
 * nothing, so a CI webhook, a soft timer, and the periodic sweep never double-resolve a
 * run. All methods are async so durable backends can do I/O.
 */
export interface ParkStore {
  /** Create or replace the record for `record.sessionId`, (re)indexing by PR key. */
  put(record: ParkRecord): Promise<void>;
  /** Return the record for `sessionId` (a copy; mutating it cannot corrupt the store), or null. */
  get(sessionId: string): Promise<ParkRecord | null>;
  /**
   * Atomically claim the run indexed by `prKey`: clear the index and return the record
   * (the per-run record is retained so a retry can read its params). Returns null for a
   * late/duplicate/unknown caller or an empty key. The returned copy carries the claimed
   * PR key so the caller can stop its timer.
   */
  resolveByPrKey(prKey: string): Promise<ParkRecord | null>;
  /** Remove the record (and any lingering index) for `sessionId`. Terminal cleanup; no-op if absent. */
  delete(sessionId: string): Promise<void>;
  /**
   * Atomically claim and return every parked record whose `parkedAt` is before `cutoff`.
   * Each is claimed exactly once (no double-winner); the returned copies keep their PR key.
   */
  sweep(cutoff: Date): Promise<ParkRecord[]>;
  /** How many records are currently parked (PR-key-indexed). */
  parkedCount(): Promise<number>;
  /** Release backing resources (durable backends). Default no-op; called on clean shutdown. */
  close(): Promise<void>;
}

/** Deep-copy a record so callers and the store never share a mutable reference. */
function cloneRecord(r: ParkRecord): ParkRecord {
  return { ...r, parkedAt: r.parkedAt === null ? null : new Date(r.parkedAt.getTime()) };
}

/**
 * In-process park store (the default backend). Holds records by session id and a PR-key
 * → session-id index of the parked subset. Records are copied in and out so a caller's
 * mutation cannot corrupt stored state, and claims are single-winner because JS's single
 * event loop never preempts between the index lookup and its removal.
 */
export class MemoryParkStore implements ParkStore {
  private readonly bySession = new Map<string, ParkRecord>();
  private readonly index = new Map<string, string>(); // prKey -> sessionId

  /** Store (or overwrite) a session's record and keep the prKey→session index single-holder. */
  put(record: ParkRecord): Promise<void> {
    const stored = cloneRecord(record);
    // Stale-index hygiene: if this session was indexed under a different key, drop the
    // old entry so a re-park under a new key cannot leave the prior key dangling.
    const prev = this.bySession.get(stored.sessionId);
    if (prev && prev.prKey !== '' && prev.prKey !== stored.prKey) {
      if (this.index.get(prev.prKey) === stored.sessionId) {
        this.index.delete(prev.prKey);
      }
    }
    this.bySession.set(stored.sessionId, stored);
    if (stored.prKey !== '') {
      this.index.set(stored.prKey, stored.sessionId);
    }
    return Promise.resolve();
  }

  /** Fetch a copy of a session's record by id, or null if it is not stored. */
  get(sessionId: string): Promise<ParkRecord | null> {
    const rec = this.bySession.get(sessionId);
    return Promise.resolve(rec ? cloneRecord(rec) : null);
  }

  /** Atomically claim the run parked under a PR key (single winner); the record is retained, unparked. */
  resolveByPrKey(prKey: string): Promise<ParkRecord | null> {
    if (prKey === '') {
      return Promise.resolve(null);
    }
    const sid = this.index.get(prKey);
    if (sid === undefined) {
      return Promise.resolve(null);
    }
    this.index.delete(prKey); // claim
    const rec = this.bySession.get(sid);
    if (!rec) {
      return Promise.resolve(null);
    }
    rec.prKey = ''; // unpark in storage; retain the record so a retry can read its params
    const out = cloneRecord(rec);
    out.prKey = prKey; // hand the claimed key back so the caller can stop its timer
    return Promise.resolve(out);
  }

  /** Remove a session's record and any index entry it still holds. */
  delete(sessionId: string): Promise<void> {
    const rec = this.bySession.get(sessionId);
    if (rec && rec.prKey !== '' && this.index.get(rec.prKey) === sessionId) {
      this.index.delete(rec.prKey);
    }
    this.bySession.delete(sessionId);
    return Promise.resolve();
  }

  /** Claim every run parked before the cutoff (each exactly once), for the timeout backstop. */
  sweep(cutoff: Date): Promise<ParkRecord[]> {
    const claimed: ParkRecord[] = [];
    for (const [key, sid] of [...this.index.entries()]) {
      const rec = this.bySession.get(sid);
      if (!rec || rec.parkedAt === null || rec.parkedAt.getTime() >= cutoff.getTime()) {
        continue;
      }
      this.index.delete(key); // claim
      const out = cloneRecord(rec);
      out.prKey = key; // keep the key for logging/cleanup
      rec.prKey = ''; // unpark in storage; retain until the caller clears it
      claimed.push(out);
    }
    return Promise.resolve(claimed);
  }

  /** Number of currently parked runs (those still holding a PR key). */
  parkedCount(): Promise<number> {
    return Promise.resolve(this.index.size);
  }

  /** No-op for the in-memory store; present to satisfy the {@link ParkStore} contract. */
  close(): Promise<void> {
    return Promise.resolve();
  }
}

/** Build the park store for the configured session backend. */
export function newParkStore(cfg: Config): ParkStore {
  switch (cfg.sessionBackend) {
    case SessionBackend.Memory:
      return new MemoryParkStore();
    case SessionBackend.Sqlite:
      // Resolve to an absolute path so the park store and the ADK sqlite session service
      // open the very same file regardless of the process working directory.
      return new SqliteParkStore(resolve(cfg.sqliteDsn));
    case SessionBackend.Firestore:
      return new FirestoreParkStore(cfg.firestoreProject, `${cfg.firestoreCollection}_parked_runs`);
  }
}
