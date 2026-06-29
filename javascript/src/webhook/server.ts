/**
 * HTTP ingress endpoints.
 *
 * Each request is reduced to a normalized {@link Envelope} and handed to an
 * {@link IngestFunc}, which should enqueue work and return quickly. Deterministic
 * tooling — no agent imports.
 */

import { createHmac, timingSafeEqual } from 'node:crypto';
import express, { type Express, type Request, type Response } from 'express';

import { type Envelope, Kind, decode, newEnvelope } from '../ingest/envelope';

/** maxBodyBytes caps how much of a webhook body we read. */
export const MAX_BODY_BYTES = 5 << 20; // 5 MiB

/** The body for internal cron triggers, which carry no payload. */
const EMPTY_BODY = Buffer.alloc(0);

/**
 * IngestFunc consumes a normalized envelope. It should enqueue work and return
 * quickly; a rejected promise becomes a 500 to the caller.
 */
export type IngestFunc = (e: Envelope) => Promise<void>;

/** SweepFunc resolves every engine's timed-out parked runs (the /internal/sweep body). */
export type SweepFunc = () => Promise<void>;

/**
 * DispatchFunc runs an envelope's workflow synchronously, in-request. It backs POST
 * /internal/dispatch, which the Cloud Tasks transport delivers to so the compute runs on
 * allocated CPU (unlike a post-202 background task). It is the root dispatcher's dispatch.
 */
export type DispatchFunc = (e: Envelope) => Promise<void>;

/** Structured logger for non-fatal handler diagnostics (e.g. an acked poison dispatch body). */
export interface Logger {
  warn(msg: string, fields?: Record<string, unknown>): void;
  error(msg: string, fields?: Record<string, unknown>): void;
}

/** A logger that drops everything — the fallback when none is injected. */
const NOOP_LOGGER: Logger = { warn() {}, error() {} };

/** Options for constructing a {@link Server}. */
export interface ServerOptions {
  /**
   * Enables HMAC verification of /webhooks/github. When empty, verification is
   * skipped (intended for local dev only).
   */
  secret?: string;
  /**
   * Bearer token guarding the /internal/* cron + sweep routes (Cloud Scheduler ingress).
   * When empty, those routes are disabled and return 404.
   */
  internalToken?: string;
  /** The sweep handler behind POST /internal/sweep. Omitted → that route returns 501. */
  sweep?: SweepFunc;
  /**
   * The synchronous, in-request executor invoked by POST /internal/dispatch (the Cloud Tasks
   * transport's worker endpoint). Omitted → that route returns 501.
   */
  dispatch?: DispatchFunc;
  /**
   * Logger for non-fatal handler diagnostics (e.g. a poison /internal/dispatch body that is
   * acked rather than retried). Omitted → a no-op logger.
   */
  log?: Logger;
  /** Injects a clock for deterministic receivedAt timestamps in tests. */
  now?: () => Date;
}

/**
 * Check a GitHub `sha256=<hex>` HMAC over the request body. The hex digest is
 * compared in constant time.
 */
export function verifySignature(secret: string, header: string, body: Buffer): boolean {
  const prefix = 'sha256=';
  if (!header.startsWith(prefix)) {
    return false;
  }
  const want = createHmac('sha256', secret).update(body).digest('hex');
  const got = header.slice(prefix.length);
  // timingSafeEqual requires equal-length buffers, so length-mismatch is rejected first.
  const wantBuf = Buffer.from(want, 'utf8');
  const gotBuf = Buffer.from(got, 'utf8');
  if (wantBuf.length !== gotBuf.length) {
    return false;
  }
  return timingSafeEqual(wantBuf, gotBuf);
}

/** Routes webhook requests to an IngestFunc. */
export class Server {
  private readonly ingest: IngestFunc;
  private readonly secret: string;
  private readonly internalToken: string;
  private readonly sweepFn?: SweepFunc;
  private readonly dispatchFn?: DispatchFunc;
  private readonly log: Logger;
  private readonly now: () => Date;
  private readonly expressApp: Express;

  constructor(ingest: IngestFunc, opts: ServerOptions = {}) {
    this.ingest = ingest;
    this.secret = opts.secret ?? '';
    this.internalToken = opts.internalToken ?? '';
    this.sweepFn = opts.sweep;
    this.dispatchFn = opts.dispatch;
    this.log = opts.log ?? NOOP_LOGGER;
    this.now = opts.now ?? (() => new Date());
    this.expressApp = this.buildApp();
  }

  /** Return the express app to mount (the `Handler()` analogue). */
  get app(): Express {
    return this.expressApp;
  }

  /** Construct the Express app: health check, webhook routes, and the token-guarded internal routes. */
  private buildApp(): Express {
    const app = express();

    app.get('/healthz', (_req: Request, res: Response) => {
      res.status(200).type('text/plain').send('ok');
    });

    // A kickoff selects the caller-supplied target repo, so /webhooks/lint and
    // /webhooks/coverage are HMAC-authenticated with the same shared secret as the
    // GitHub webhook (verification is skipped only when no secret is set).
    app.post('/webhooks/lint', (req: Request, res: Response) => {
      void this.handleBody(req, res, (body) => {
        if (!this.authenticated(req, res, body)) {
          return Promise.resolve();
        }
        return this.dispatch(res, newEnvelope(Kind.Lint, 'webhook:/lint', body, this.now()));
      });
    });

    app.post('/webhooks/coverage', (req: Request, res: Response) => {
      void this.handleBody(req, res, (body) => {
        if (!this.authenticated(req, res, body)) {
          return Promise.resolve();
        }
        return this.dispatch(res, newEnvelope(Kind.Coverage, 'webhook:/coverage', body, this.now()));
      });
    });

    app.post('/webhooks/github', (req: Request, res: Response) => {
      void this.handleBody(req, res, (body) => {
        if (!this.authenticated(req, res, body)) {
          return Promise.resolve();
        }
        return this.dispatch(res, newEnvelope(Kind.CI, 'webhook:/github', body, this.now()));
      });
    });

    // Internal ingress (Cloud Scheduler): the daily digest trigger + the durable timeout
    // sweep, guarded by a Bearer token. Disabled (404) until INTERNAL_TOKEN is set. Driving
    // the daily digest GCP-side lets a scaled-to-zero deployment fire it without an internal timer.
    app.post('/internal/cron/daily', (req: Request, res: Response) => {
      if (!this.internalAuthenticated(req, res)) {
        return;
      }
      void this.dispatch(res, newEnvelope(Kind.CronDaily, 'internal:/cron/daily', EMPTY_BODY, this.now()));
    });

    app.post('/internal/sweep', (req: Request, res: Response) => {
      if (!this.internalAuthenticated(req, res)) {
        return;
      }
      void this.handleSweep(res);
    });

    // Cloud Tasks worker: executes a queued envelope in-request (same Bearer auth). Running
    // in-request (not in a post-202 background task) keeps CPU allocated on Cloud Run for the
    // whole compute. Retry classification follows Cloud Tasks' retry-on-non-2xx contract
    // (spec §6): a transient failure (the dispatch rejects — LLM/network/Firestore) returns 500
    // so the queue retries with backoff; a permanent failure (a malformed body or unknown kind,
    // which a retry cannot fix) is acked with 200 and logged so the queue drops the poison task
    // instead of looping.
    app.post('/internal/dispatch', (req: Request, res: Response) => {
      if (!this.internalAuthenticated(req, res)) {
        return;
      }
      if (!this.dispatchFn) {
        res.status(501).type('text/plain').send('dispatch not configured');
        return;
      }
      void this.handleBody(req, res, (body) => this.handleDispatch(res, body));
    });

    return app;
  }

  /** Run a queued envelope in-request, mapping a poison body to an acked 200 and a transient
   * dispatch error to 500. Reached only once a dispatch handler is configured. */
  private async handleDispatch(res: Response, body: Buffer): Promise<void> {
    if (!this.dispatchFn) {
      res.status(501).type('text/plain').send('dispatch not configured');
      return;
    }
    let env: Envelope;
    try {
      env = decode(body);
    } catch (err) {
      // Permanent: ack so Cloud Tasks does not redeliver a poison payload.
      this.log.warn('dropping undecodable dispatch task', { err: (err as Error).message });
      res.status(200).end();
      return;
    }
    try {
      await this.dispatchFn(env);
    } catch (err) {
      // Transient: let Cloud Tasks retry with backoff.
      this.log.error('dispatch failed', {
        kind: env.kind,
        source: env.source,
        err: (err as Error).message,
      });
      res.status(500).type('text/plain').send('dispatch failed');
      return;
    }
    res.status(200).end();
  }

  /**
   * Guard an /internal/* route with the Bearer token. Writes the response and returns false
   * when denied: 404 if internal routes are disabled (no token configured), 401 on a missing
   * or mismatched token (compared in constant time).
   */
  private internalAuthenticated(req: Request, res: Response): boolean {
    if (this.internalToken === '') {
      res.status(404).type('text/plain').send('internal endpoints disabled');
      return false;
    }
    const prefix = 'Bearer ';
    const auth = headerValue(req, 'authorization');
    if (!auth.startsWith(prefix)) {
      res.status(401).type('text/plain').send('unauthorized');
      return false;
    }
    const got = Buffer.from(auth.slice(prefix.length), 'utf8');
    const want = Buffer.from(this.internalToken, 'utf8');
    if (got.length !== want.length || !timingSafeEqual(got, want)) {
      res.status(401).type('text/plain').send('unauthorized');
      return false;
    }
    return true;
  }

  /** Run the configured sweep, mapping a missing handler to 501 and a sweep error to 500. */
  private async handleSweep(res: Response): Promise<void> {
    if (!this.sweepFn) {
      res.status(501).type('text/plain').send('sweep not configured');
      return;
    }
    try {
      await this.sweepFn();
    } catch {
      res.status(500).type('text/plain').send('sweep failed');
      return;
    }
    res.status(200).end();
  }

  /**
   * Verify the request's HMAC signature when a secret is configured, writing a 401 and
   * returning false on mismatch. With no secret (local dev only) every request passes.
   */
  private authenticated(req: Request, res: Response, body: Buffer): boolean {
    if (this.secret === '') {
      return true;
    }
    if (!verifySignature(this.secret, headerValue(req, 'x-hub-signature-256'), body)) {
      res.status(401).type('text/plain').send('invalid signature');
      return false;
    }
    return true;
  }

  /**
   * Read the body (with the cap) then run `next`, mapping an oversize body to 413 and any
   * other read error to 400.
   */
  private async handleBody(
    req: Request,
    res: Response,
    next: (body: Buffer) => Promise<void>,
  ): Promise<void> {
    let body: Buffer;
    try {
      body = await readBody(req);
    } catch (err) {
      if (err instanceof BodyTooLargeError) {
        res.status(413).type('text/plain').send('request body too large');
      } else {
        res.status(400).type('text/plain').send('read body');
      }
      return;
    }
    await next(body);
  }

  /** Hand an envelope to the ingest handler and translate the outcome into an HTTP status. */
  private async dispatch(res: Response, env: Envelope): Promise<void> {
    try {
      await this.ingest(env);
    } catch {
      res.status(500).type('text/plain').send('ingest failed');
      return;
    }
    res.status(202).end();
  }
}

/** The header lookup is case-insensitive; express normalizes to lowercase. */
function headerValue(req: Request, name: string): string {
  const v = req.headers[name];
  if (Array.isArray(v)) {
    return v[0] ?? '';
  }
  return v ?? '';
}

/** Raised by {@link readBody} when a body exceeds MAX_BODY_BYTES. Mapped to a 413. */
class BodyTooLargeError extends Error {}

/**
 * Read up to MAX_BODY_BYTES. A body over the cap is rejected (BodyTooLargeError → 413)
 * rather than silently truncated — a truncated body would both fail HMAC verification and
 * feed malformed JSON downstream. Rejects with the transport error on a read failure.
 */
function readBody(req: Request): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    let done = false;

    const fail = (err: Error): void => {
      if (done) {
        return;
      }
      done = true;
      // Stop buffering further data but leave the socket alone so the caller can still
      // write its 413/400 response; destroying it here would surface as a socket hang-up.
      req.resume();
      reject(err);
    };

    req.on('data', (chunk: Buffer) => {
      if (done) {
        return;
      }
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        fail(new BodyTooLargeError('request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (done) {
        return;
      }
      done = true;
      resolve(Buffer.concat(chunks));
    });
    req.on('error', (err: Error) => {
      fail(err);
    });
  });
}
