/**
 * The sqlite park store: a durable {@link ParkStore} on the built-in `node:sqlite`.
 *
 * Parked-run records live in a `parked_runs` table in the same file the ADK sqlite
 * session service uses (one `SQLITE_DSN`), so a parked run's session history and its
 * park record share a backend. The connection runs in WAL mode with a busy timeout so
 * it coexists with the session service's separate connection on that file.
 *
 * Concurrency: `node:sqlite` is synchronous, so every method completes its statements
 * without yielding the event loop — a select-then-CAS-update is atomic within this
 * process, and the `UPDATE … WHERE session_id=? AND pr_key=?` compare-and-set makes the
 * claim single-winner even against another process. No extra lock is needed.
 */
import { createRequire } from 'node:module';
import type { DatabaseSync as DatabaseSyncInstance } from 'node:sqlite';

import { type ParkRecord, type ParkStore } from './parkstore';

// node:sqlite is a recent built-in that vite's bundler (used by vitest) does not yet
// recognize — a static `import 'node:sqlite'` gets rewritten to a bare `sqlite` and fails
// to resolve. Loading it through a real Node require sidesteps the bundler entirely; the
// value side is required here while the type side comes from the erased `import type`.
const nodeRequire = createRequire(import.meta.url);
const { DatabaseSync } = nodeRequire('node:sqlite') as typeof import('node:sqlite');

interface Row {
  session_id: string;
  pr_key: string;
  call_id: string;
  attempts: number | bigint;
  params: string;
  parked_at: number | bigint | null;
}

function rowToRecord(row: Row): ParkRecord {
  return {
    sessionId: row.session_id,
    prKey: row.pr_key,
    callId: row.call_id,
    attempts: Number(row.attempts),
    params: row.params,
    parkedAt: row.parked_at === null ? null : new Date(Number(row.parked_at)),
  };
}

/** A durable park store backed by a sqlite file via the built-in `node:sqlite`. */
export class SqliteParkStore implements ParkStore {
  private readonly db: DatabaseSyncInstance;
  private closed = false;

  constructor(dsn: string) {
    this.db = new DatabaseSync(dsn);
    // WAL + a busy timeout so this connection coexists with the session service's
    // separate connection on the same file without spurious "database is locked".
    this.db.exec('PRAGMA journal_mode = WAL;');
    this.db.exec('PRAGMA busy_timeout = 5000;');
    this.db.exec(
      `CREATE TABLE IF NOT EXISTS parked_runs (
         session_id TEXT PRIMARY KEY,
         pr_key     TEXT    NOT NULL DEFAULT '',
         call_id    TEXT    NOT NULL DEFAULT '',
         attempts   INTEGER NOT NULL DEFAULT 0,
         params     TEXT    NOT NULL DEFAULT '',
         parked_at  INTEGER
       );`,
    );
    // pr_key doubles as the resume index ('' when not parked); a re-park under a new key
    // overwrites the row's key, so an old key never lingers.
    this.db.exec('CREATE INDEX IF NOT EXISTS idx_parked_runs_pr_key ON parked_runs(pr_key);');
  }

  put(record: ParkRecord): Promise<void> {
    this.db
      .prepare(
        `INSERT OR REPLACE INTO parked_runs
           (session_id, pr_key, call_id, attempts, params, parked_at)
         VALUES (?, ?, ?, ?, ?, ?);`,
      )
      .run(
        record.sessionId,
        record.prKey,
        record.callId,
        record.attempts,
        record.params,
        record.parkedAt === null ? null : record.parkedAt.getTime(),
      );
    return Promise.resolve();
  }

  get(sessionId: string): Promise<ParkRecord | null> {
    const row = this.db
      .prepare('SELECT * FROM parked_runs WHERE session_id = ?;')
      .get(sessionId) as Row | undefined;
    return Promise.resolve(row ? rowToRecord(row) : null);
  }

  resolveByPrKey(prKey: string): Promise<ParkRecord | null> {
    if (prKey === '') {
      return Promise.resolve(null);
    }
    const row = this.db
      .prepare('SELECT * FROM parked_runs WHERE pr_key = ? LIMIT 1;')
      .get(prKey) as Row | undefined;
    if (!row) {
      return Promise.resolve(null);
    }
    // CAS: only the caller that still sees the key wins; a racing claimer gets changes=0.
    const res = this.db
      .prepare("UPDATE parked_runs SET pr_key = '' WHERE session_id = ? AND pr_key = ?;")
      .run(row.session_id, prKey);
    if (Number(res.changes) !== 1) {
      return Promise.resolve(null);
    }
    const rec = rowToRecord(row);
    rec.prKey = prKey; // the row is retained unparked; hand the claimed key back to the caller
    return Promise.resolve(rec);
  }

  delete(sessionId: string): Promise<void> {
    this.db.prepare('DELETE FROM parked_runs WHERE session_id = ?;').run(sessionId);
    return Promise.resolve();
  }

  sweep(cutoff: Date): Promise<ParkRecord[]> {
    const rows = this.db
      .prepare("SELECT * FROM parked_runs WHERE pr_key <> '' AND parked_at IS NOT NULL AND parked_at < ?;")
      .all(cutoff.getTime()) as unknown as Row[];
    const claimed: ParkRecord[] = [];
    for (const row of rows) {
      const res = this.db
        .prepare("UPDATE parked_runs SET pr_key = '' WHERE session_id = ? AND pr_key = ?;")
        .run(row.session_id, row.pr_key);
      if (Number(res.changes) === 1) {
        const rec = rowToRecord(row);
        rec.prKey = row.pr_key; // keep the key for logging/cleanup
        claimed.push(rec);
      }
    }
    return Promise.resolve(claimed);
  }

  parkedCount(): Promise<number> {
    const row = this.db
      .prepare("SELECT COUNT(*) AS n FROM parked_runs WHERE pr_key <> '';")
      .get() as { n: number | bigint };
    return Promise.resolve(Number(row.n));
  }

  close(): Promise<void> {
    // Idempotent: node:sqlite throws on a second close, but shutdown paths may call it more
    // than once (e.g. a test that closes explicitly plus a teardown).
    if (!this.closed) {
      this.closed = true;
      this.db.close();
    }
    return Promise.resolve();
  }
}
