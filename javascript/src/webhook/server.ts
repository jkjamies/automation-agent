/**
 * HTTP ingress endpoints.
 *
 * Each request is reduced to a normalized {@link Envelope} and handed to an
 * {@link IngestFunc}, which should enqueue work and return quickly. Deterministic
 * tooling — no agent imports.
 */

import { createHmac, timingSafeEqual } from 'node:crypto';
import express, { type Express, type Request, type Response } from 'express';

import { type Envelope, Kind, newEnvelope } from '../ingest/envelope';

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
  private readonly now: () => Date;
  private readonly expressApp: Express;

  constructor(ingest: IngestFunc, opts: ServerOptions = {}) {
    this.ingest = ingest;
    this.secret = opts.secret ?? '';
    this.internalToken = opts.internalToken ?? '';
    this.sweepFn = opts.sweep;
    this.now = opts.now ?? (() => new Date());
    this.expressApp = this.buildApp();
  }

  /** Return the express app to mount (the `Handler()` analogue). */
  get app(): Express {
    return this.expressApp;
  }

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

    // Internal ingress (Cloud Scheduler): cron triggers + the durable timeout sweep, guarded
    // by a Bearer token. Disabled (404) until INTERNAL_TOKEN is set. The cron routes mirror
    // the in-process scheduler so a scaled-to-zero deployment can drive the digests externally.
    app.post('/internal/cron/daily', (req: Request, res: Response) => {
      if (!this.internalAuthenticated(req, res)) {
        return;
      }
      void this.dispatch(res, newEnvelope(Kind.CronDaily, 'internal:/cron/daily', EMPTY_BODY, this.now()));
    });

    app.post('/internal/cron/weekly', (req: Request, res: Response) => {
      if (!this.internalAuthenticated(req, res)) {
        return;
      }
      void this.dispatch(res, newEnvelope(Kind.CronWeekly, 'internal:/cron/weekly', EMPTY_BODY, this.now()));
    });

    app.post('/internal/sweep', (req: Request, res: Response) => {
      if (!this.internalAuthenticated(req, res)) {
        return;
      }
      void this.handleSweep(res);
    });

    return app;
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
