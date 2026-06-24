/**
 * The firestore park store: the serverless, scale-to-zero {@link ParkStore} backend.
 *
 * Park records are documents keyed by session id; `pr_key` doubles as the resume index
 * ('' when not parked), so re-parking under a new key cannot leak a stale entry. The
 * atomic claim ({@link FirestoreParkStore.resolveByPrKey}) runs in a firestore
 * transaction: of N concurrent resolvers the first to commit clears `pr_key`; the others
 * retry, re-read the cleared key, and find nothing — so exactly one wins. The doc field
 * names are snake_case to match the Go reference's schema.
 *
 * The `@google-cloud/firestore` client is heavy and only this backend needs it, so it is
 * loaded lazily through a real Node require when a store is constructed (the type side
 * comes from an erased `import type`). This file is excluded from the default coverage
 * gate and exercised only under the firestore emulator.
 */
import { createRequire } from 'node:module';
import type { Firestore, Timestamp } from '@google-cloud/firestore';

import { type ParkRecord, type ParkStore } from './parkstore';

const nodeRequire = createRequire(import.meta.url);

interface ParkDoc {
  session_id: string;
  pr_key: string;
  call_id: string;
  attempts: number;
  params: string;
  parked_at: Timestamp | null;
}

function docToRecord(d: ParkDoc): ParkRecord {
  return {
    sessionId: d.session_id,
    prKey: d.pr_key,
    callId: d.call_id,
    attempts: d.attempts,
    params: d.params,
    parkedAt: d.parked_at ? d.parked_at.toDate() : null,
  };
}

// firestore accepts a JS Date and stores it as a Timestamp, so writes pass the Date through.
function recordToDoc(r: ParkRecord): Record<string, unknown> {
  return {
    session_id: r.sessionId,
    pr_key: r.prKey,
    call_id: r.callId,
    attempts: r.attempts,
    params: r.params,
    parked_at: r.parkedAt,
  };
}

/** A durable park store backed by Cloud Firestore. */
export class FirestoreParkStore implements ParkStore {
  private readonly db: Firestore;
  private readonly collection: string;

  constructor(project: string, collection: string) {
    const { Firestore: FirestoreCtor } = nodeRequire(
      '@google-cloud/firestore',
    ) as typeof import('@google-cloud/firestore');
    // An empty project lets the client detect it from ADC / GOOGLE_CLOUD_PROJECT / emulator.
    this.db = project ? new FirestoreCtor({ projectId: project }) : new FirestoreCtor();
    this.collection = collection;
  }

  private col() {
    return this.db.collection(this.collection);
  }

  async put(record: ParkRecord): Promise<void> {
    if (record.prKey !== '') {
      // One active doc per pr_key: clear it on any OTHER session still holding it, so
      // resolve/sweep have a single winner. Best-effort (not transactional with the set).
      const dupes = await this.col().where('pr_key', '==', record.prKey).get();
      for (const d of dupes.docs) {
        if (d.id !== record.sessionId) {
          await d.ref.update({ pr_key: '' });
        }
      }
    }
    await this.col().doc(record.sessionId).set(recordToDoc(record));
  }

  async get(sessionId: string): Promise<ParkRecord | null> {
    const snap = await this.col().doc(sessionId).get();
    if (!snap.exists) {
      return null;
    }
    return docToRecord(snap.data() as ParkDoc);
  }

  resolveByPrKey(prKey: string): Promise<ParkRecord | null> {
    if (prKey === '') {
      return Promise.resolve(null); // an empty key would match unparked docs (pr_key='')
    }
    return this.db.runTransaction(async (tx) => {
      const snap = await tx.get(this.col().where('pr_key', '==', prKey).limit(1));
      if (snap.empty) {
        return null;
      }
      const doc = snap.docs[0]!;
      const d = doc.data() as ParkDoc;
      if (d.pr_key === '') {
        return null; // already claimed by a racing resolver
      }
      tx.update(doc.ref, { pr_key: '' });
      const rec = docToRecord(d);
      rec.prKey = prKey; // hand the claimed key back so the caller can stop its timer
      return rec;
    });
  }

  async delete(sessionId: string): Promise<void> {
    await this.col().doc(sessionId).delete();
  }

  async sweep(cutoff: Date): Promise<ParkRecord[]> {
    // Collect candidates (parked + stale) from a single scan, then claim each in its own
    // transaction so a concurrent resolve cannot double-claim. parked_at is filtered in
    // code (not in the query) to avoid a composite index on (pr_key, parked_at).
    const snap = await this.col().where('pr_key', '!=', '').get();
    const candidates: { sessionId: string; prKey: string }[] = [];
    for (const d of snap.docs) {
      const doc = d.data() as ParkDoc;
      if (doc.parked_at && doc.parked_at.toDate().getTime() < cutoff.getTime()) {
        candidates.push({ sessionId: doc.session_id, prKey: doc.pr_key });
      }
    }

    const out: ParkRecord[] = [];
    const errors: unknown[] = [];
    for (const c of candidates) {
      // A per-doc error skips that candidate (it stays parked for the next sweep) rather
      // than discarding the records already claimed this pass.
      try {
        const rec = await this.claimStaleBySession(c.sessionId, c.prKey, cutoff);
        if (rec) {
          out.push(rec);
        }
      } catch (err) {
        errors.push(err);
      }
    }
    // Return everything claimed this pass even if some candidates failed: a claimed record's
    // pr_key is already cleared, so dropping it here would strand it (unparked yet never
    // notified). Failed candidates stay parked and are retried next sweep; throw only when
    // nothing was claimed, so the handler 500s and Cloud Scheduler retries.
    if (out.length === 0 && errors.length > 0) {
      throw new AggregateError(errors, 'firestore sweep: all claims failed');
    }
    return out;
  }

  // The sweep's per-doc atomic claim, keyed by session id. Inside the transaction it
  // re-checks that the doc still carries the expected (stale) pr_key and is still older
  // than the cutoff, so a resolve+re-park between the scan and the claim leaves the fresh
  // park untouched instead of clearing it with a false timeout.
  private claimStaleBySession(
    sid: string,
    prKey: string,
    cutoff: Date,
  ): Promise<ParkRecord | null> {
    return this.db.runTransaction(async (tx) => {
      const snap = await tx.get(this.col().doc(sid));
      if (!snap.exists) {
        return null;
      }
      const d = snap.data() as ParkDoc;
      if (d.pr_key !== prKey || !d.parked_at || d.parked_at.toDate().getTime() >= cutoff.getTime()) {
        return null; // resolved and/or re-parked since the scan — not ours to sweep
      }
      tx.update(snap.ref, { pr_key: '' });
      const rec = docToRecord(d);
      rec.prKey = prKey; // restore for the caller (the timeout sweep needs the PR)
      return rec;
    });
  }

  async parkedCount(): Promise<number> {
    const agg = await this.col().where('pr_key', '!=', '').count().get();
    return agg.data().count;
  }

  async close(): Promise<void> {
    await this.db.terminate();
  }
}
