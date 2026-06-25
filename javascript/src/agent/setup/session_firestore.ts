/**
 * A custom ADK session service backed by Cloud Firestore — the cloud-durable session store
 * (adk-js ships only in-memory/database/vertex services). It mirrors the in-memory
 * service's semantics: app:/user:/temp: state scopes, partial-event skipping, and event
 * filtering on read.
 *
 * Events are stored in an `events` sub-collection (one JSON-blob doc per event, ordered by
 * sequence) rather than an array field, so a long-lived session cannot blow Firestore's
 * 1 MiB per-document limit. App and user state live in their own collections; the session
 * doc holds only session-scoped state. Create and appendEvent run in a transaction so the
 * session, its scoped state, and its events advance together.
 *
 * The `@google-cloud/firestore` client is loaded lazily through a real Node require (the
 * type side comes from an erased `import type`) so only this backend pulls it in. This file
 * is excluded from the default coverage gate and exercised only under the firestore emulator.
 */
import { randomUUID } from 'node:crypto';
import { createRequire } from 'node:module';
import type { DocumentReference, Firestore, Transaction } from '@google-cloud/firestore';

import {
  BaseSessionService,
  type CreateSessionRequest,
  type DeleteSessionRequest,
  type Event,
  type GetSessionRequest,
  type ListSessionsRequest,
  type ListSessionsResponse,
  type Session,
  createSession,
  mergeStates,
  State,
  trimTempDeltaState,
} from '@google/adk';

const nodeRequire = createRequire(import.meta.url);

type StateMap = Record<string, unknown>;

interface SessionDoc {
  app_name: string;
  user_id: string;
  session_id: string;
  state: StateMap;
  next_seq: number;
  updated_at: number; // epoch ms of the last event
}

interface EventDoc {
  seq: number;
  timestamp: number; // epoch ms (for the afterTimestamp filter)
  blob: string; // JSON-encoded Event
}

const KEY_DELIM = '\x1f';

/** A Firestore-safe document id: base64url of the delimiter-joined parts. */
function encodeKey(...parts: string[]): string {
  return Buffer.from(parts.join(KEY_DELIM)).toString('base64url');
}

/**
 * Split a state delta into its app / user / session scopes, stripping the scope prefix.
 * temp: keys are dropped (never persisted), mirroring the in-memory service.
 */
function extractStateDeltas(delta: StateMap): { app: StateMap; user: StateMap; sess: StateMap } {
  const app: StateMap = {};
  const user: StateMap = {};
  const sess: StateMap = {};
  for (const [k, v] of Object.entries(delta)) {
    if (k.startsWith(State.APP_PREFIX)) {
      app[k.slice(State.APP_PREFIX.length)] = v;
    } else if (k.startsWith(State.USER_PREFIX)) {
      user[k.slice(State.USER_PREFIX.length)] = v;
    } else if (!k.startsWith(State.TEMP_PREFIX)) {
      sess[k] = v;
    }
  }
  return { app, user, sess };
}

/** Apply the Get request's numRecentEvents / afterTimestamp filters (events are seq-ordered). */
function filterEvents(events: Event[], numRecent?: number, after?: number): Event[] {
  let out = events;
  if (numRecent && numRecent > 0 && out.length > numRecent) {
    out = out.slice(out.length - numRecent);
  }
  if (after && after > 0) {
    out = out.filter((e) => e.timestamp >= after);
  }
  return out;
}

/** A Firestore-backed ADK session service. */
export class FirestoreSessionService extends BaseSessionService {
  private readonly db: Firestore;
  private readonly sessionsColl: string;
  private readonly appStateColl: string;
  private readonly userStateColl: string;

  constructor(project: string, prefix: string) {
    super();
    const { Firestore: FirestoreCtor } = nodeRequire(
      '@google-cloud/firestore',
    ) as typeof import('@google-cloud/firestore');
    this.db = project ? new FirestoreCtor({ projectId: project }) : new FirestoreCtor();
    this.sessionsColl = `${prefix}_sessions`;
    this.appStateColl = `${prefix}_app_state`;
    this.userStateColl = `${prefix}_user_state`;
  }

  /** Release the underlying Firestore client. */
  async close(): Promise<void> {
    await this.db.terminate();
  }

  /** Doc reference for a single session, keyed by (app, user, session id). */
  private sessionRef(app: string, user: string, sid: string): DocumentReference {
    return this.db.collection(this.sessionsColl).doc(encodeKey(app, user, sid));
  }

  /** Doc reference for an app's shared state. */
  private appStateRef(app: string): DocumentReference {
    return this.db.collection(this.appStateColl).doc(encodeKey(app));
  }

  /** Doc reference for a user's per-app state. */
  private userStateRef(app: string, user: string): DocumentReference {
    return this.db.collection(this.userStateColl).doc(encodeKey(app, user));
  }

  /** Read a state map from a doc inside a transaction, defaulting to empty when the doc is absent. */
  private async loadStateInTx(tx: Transaction, ref: DocumentReference): Promise<StateMap> {
    const snap = await tx.get(ref);
    if (!snap.exists) {
      return {};
    }
    return ((snap.data() as { state?: StateMap }).state ?? {}) as StateMap;
  }

  /** Read a state map from a doc (non-transactional), defaulting to empty when the doc is absent. */
  private async loadState(ref: DocumentReference): Promise<StateMap> {
    const snap = await ref.get();
    if (!snap.exists) {
      return {};
    }
    return ((snap.data() as { state?: StateMap }).state ?? {}) as StateMap;
  }

  /**
   * Create a session and merge any app/user state deltas in one transaction, so a session is never
   * persisted without its state (or vice versa). Generates a session id when the request omits one.
   */
  async createSession(req: CreateSessionRequest): Promise<Session> {
    if (!req.appName || !req.userId) {
      throw new Error(
        `appName and userId are required, got appName=${JSON.stringify(req.appName)} userId=${JSON.stringify(req.userId)}`,
      );
    }
    const sid = req.sessionId && req.sessionId !== '' ? req.sessionId : randomUUID();
    const { app: appDelta, user: userDelta, sess: sessDelta } = extractStateDeltas(req.state ?? {});
    const now = Date.now();
    const ref = this.sessionRef(req.appName, req.userId, sid);
    const appRef = this.appStateRef(req.appName);
    const userRef = this.userStateRef(req.appName, req.userId);

    // One transaction creates the session and merges app/user state, so a state write can no
    // longer leave a session persisted without its state (or vice versa). All reads precede
    // all writes, as Firestore transactions require.
    const { appState, userState } = await this.db.runTransaction(async (tx) => {
      const existing = await tx.get(ref);
      if (existing.exists) {
        throw new Error(`session ${sid} already exists`);
      }
      const appState = await this.loadStateInTx(tx, appRef);
      const userState = await this.loadStateInTx(tx, userRef);
      Object.assign(appState, appDelta);
      Object.assign(userState, userDelta);

      const doc: SessionDoc = {
        app_name: req.appName,
        user_id: req.userId,
        session_id: sid,
        state: sessDelta,
        next_seq: 0,
        updated_at: now,
      };
      tx.create(ref, doc);
      if (Object.keys(appDelta).length > 0) {
        tx.set(appRef, { state: appState });
      }
      if (Object.keys(userDelta).length > 0) {
        tx.set(userRef, { state: userState });
      }
      return { appState, userState };
    });

    return createSession({
      id: sid,
      appName: req.appName,
      userId: req.userId,
      state: mergeStates(appState, userState, sessDelta),
      events: [],
      lastUpdateTime: now,
    });
  }

  /** Load a session with its (optionally filtered) event history, merged over app/user/session state. */
  async getSession(req: GetSessionRequest): Promise<Session | undefined> {
    const snap = await this.sessionRef(req.appName, req.userId, req.sessionId).get();
    if (!snap.exists) {
      return undefined;
    }
    const doc = snap.data() as SessionDoc;
    const events = await this.loadEvents(snap.ref);
    const filtered = filterEvents(events, req.config?.numRecentEvents, req.config?.afterTimestamp);
    const appState = await this.loadState(this.appStateRef(req.appName));
    const userState = await this.loadState(this.userStateRef(req.appName, req.userId));
    return createSession({
      id: req.sessionId,
      appName: req.appName,
      userId: req.userId,
      state: mergeStates(appState, userState, doc.state ?? {}),
      events: filtered,
      lastUpdateTime: doc.updated_at,
    });
  }

  /** Read a session's events from its `events` sub-collection, ordered by sequence number. */
  private async loadEvents(sessionRef: DocumentReference): Promise<Event[]> {
    const docs = await sessionRef.collection('events').orderBy('seq', 'asc').get();
    return docs.docs.map((d) => JSON.parse((d.data() as EventDoc).blob) as Event);
  }

  /** List an app's sessions (optionally scoped to a user), each without its event history. */
  async listSessions(req: ListSessionsRequest): Promise<ListSessionsResponse> {
    if (!req.appName) {
      throw new Error(`appName is required, got ${JSON.stringify(req.appName)}`);
    }
    let query = this.db.collection(this.sessionsColl).where('app_name', '==', req.appName);
    if (req.userId) {
      query = query.where('user_id', '==', req.userId);
    }
    const snap = await query.get();
    const sessions: Session[] = [];
    for (const d of snap.docs) {
      const doc = d.data() as SessionDoc;
      const appState = await this.loadState(this.appStateRef(doc.app_name));
      const userState = await this.loadState(this.userStateRef(doc.app_name, doc.user_id));
      sessions.push(
        createSession({
          id: doc.session_id,
          appName: doc.app_name,
          userId: doc.user_id,
          state: mergeStates(appState, userState, doc.state ?? {}),
          events: [], // List returns sessions without their event history
          lastUpdateTime: doc.updated_at,
        }),
      );
    }
    const totalItems = sessions.length;
    return { sessions, page: 1, limit: totalItems, totalItems, totalPages: totalItems === 0 ? 0 : 1 };
  }

  /** Delete a session and its events sub-collection (Firestore does not cascade). */
  async deleteSession(req: DeleteSessionRequest): Promise<void> {
    const ref = this.sessionRef(req.appName, req.userId, req.sessionId);
    // Firestore does not cascade: delete the events sub-collection before the session doc.
    const events = await ref.collection('events').get();
    for (const ev of events.docs) {
      await ev.ref.delete();
    }
    await ref.delete();
  }

  override async appendEvent({ session, event }: { session: Session; event: Event }): Promise<Event> {
    if (event.partial) {
      return event; // partial events are not persisted
    }
    const stored = trimTempDeltaState(event);
    const { app: appDelta, user: userDelta, sess: sessDelta } = extractStateDeltas(
      stored.actions?.stateDelta ?? {},
    );
    const blob = JSON.stringify(stored);
    const ref = this.sessionRef(session.appName, session.userId, session.id);
    const appRef = this.appStateRef(session.appName);
    const userRef = this.userStateRef(session.appName, session.userId);

    await this.db.runTransaction(async (tx) => {
      const snap = await tx.get(ref);
      if (!snap.exists) {
        throw new Error('session not found, cannot apply event');
      }
      const appState = await this.loadStateInTx(tx, appRef);
      const userState = await this.loadStateInTx(tx, userRef);
      const doc = snap.data() as SessionDoc;
      const sessionState = doc.state ?? {};
      Object.assign(sessionState, sessDelta);
      const seq = doc.next_seq;

      tx.set(ref, {
        ...doc,
        state: sessionState,
        next_seq: seq + 1,
        updated_at: event.timestamp,
      } satisfies SessionDoc);
      if (Object.keys(appDelta).length > 0) {
        Object.assign(appState, appDelta);
        tx.set(appRef, { state: appState });
      }
      if (Object.keys(userDelta).length > 0) {
        Object.assign(userState, userDelta);
        tx.set(userRef, { state: userState });
      }
      const evRef = ref.collection('events').doc(String(seq).padStart(20, '0'));
      tx.set(evRef, { seq, timestamp: event.timestamp, blob } satisfies EventDoc);
    });

    // Reflect the append on the caller's in-memory session, mirroring the in-memory service:
    // the full (temp-stripped) delta, so app:/user:-prefixed keys are visible too.
    for (const [k, v] of Object.entries(stored.actions?.stateDelta ?? {})) {
      session.state[k] = v;
    }
    session.events.push(stored);
    session.lastUpdateTime = event.timestamp;
    return stored;
  }
}
