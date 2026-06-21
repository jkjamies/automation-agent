// Tests for the webhook HTTP server using supertest against the express app.
import { createHmac } from 'node:crypto';
import request from 'supertest';
import { describe, expect, it } from 'vitest';

import { type Envelope, Kind } from '../ingest/envelope';
import { MAX_BODY_BYTES, Server, verifySignature } from './server';

/** Records the last Envelope; optionally throws to force a 500. */
class Capture {
  env: Envelope | null = null;
  constructor(private readonly err: Error | null = null) {}

  ingest = async (e: Envelope): Promise<void> => {
    this.env = e;
    if (this.err !== null) {
      throw this.err;
    }
  };
}

function sign(secret: string, body: string): string {
  return 'sha256=' + createHmac('sha256', secret).update(body).digest('hex');
}

describe('webhook server', () => {
  it('lint kickoff -> 202 with KindLint', async () => {
    const c = new Capture();
    const resp = await request(new Server(c.ingest).app)
      .post('/webhooks/lint')
      .send('{"problems":[]}');

    expect(resp.status).toBe(202);
    expect(c.env).not.toBeNull();
    expect(c.env!.kind).toBe(Kind.Lint);
    expect(c.env!.source).toBe('webhook:/lint');
    expect(c.env!.payload.toString()).toBe('{"problems":[]}');
  });

  it('coverage kickoff -> 202 with KindCoverage', async () => {
    const c = new Capture();
    const resp = await request(new Server(c.ingest).app)
      .post('/webhooks/coverage')
      .send('{"report":"jacoco"}');

    expect(resp.status).toBe(202);
    expect(c.env!.kind).toBe(Kind.Coverage);
    expect(c.env!.source).toBe('webhook:/coverage');
  });

  it('github with a valid signature -> 202 with KindCI', async () => {
    const c = new Capture();
    const body = '{"action":"completed"}';
    const resp = await request(new Server(c.ingest, { secret: 'topsecret' }).app)
      .post('/webhooks/github')
      .set('X-Hub-Signature-256', sign('topsecret', body))
      .send(body);

    expect(resp.status).toBe(202);
    expect(c.env!.kind).toBe(Kind.CI);
  });

  it('github with an invalid signature -> 401', async () => {
    const c = new Capture();
    const resp = await request(new Server(c.ingest, { secret: 'topsecret' }).app)
      .post('/webhooks/github')
      .set('X-Hub-Signature-256', 'sha256=deadbeef')
      .send('{}');

    expect(resp.status).toBe(401);
  });

  it('github with a missing signature -> 401', async () => {
    const c = new Capture();
    const resp = await request(new Server(c.ingest, { secret: 'topsecret' }).app)
      .post('/webhooks/github')
      .send('{}');

    expect(resp.status).toBe(401);
  });

  it('github with no configured secret skips verification -> 202', async () => {
    const c = new Capture();
    const resp = await request(new Server(c.ingest).app).post('/webhooks/github').send('{}');

    expect(resp.status).toBe(202);
    expect(c.env!.kind).toBe(Kind.CI);
  });

  it('an ingest error becomes a 500', async () => {
    const c = new Capture(new Error('boom'));
    const resp = await request(new Server(c.ingest).app).post('/webhooks/lint').send('{}');

    expect(resp.status).toBe(500);
  });

  it('healthz -> 200 "ok"', async () => {
    const resp = await request(new Server(new Capture().ingest).app).get('/healthz');

    expect(resp.status).toBe(200);
    expect(resp.text).toBe('ok');
  });

  it('wrong method on a webhook route -> 404 (express has no method route)', async () => {
    // express returns 404 for an unmatched method/path; the body never reaches ingest.
    const resp = await request(new Server(new Capture().ingest).app).get('/webhooks/lint');

    expect(resp.status).toBe(404);
  });

  it('unknown route -> 404', async () => {
    const resp = await request(new Server(new Capture().ingest).app).post('/webhooks/nope');

    expect(resp.status).toBe(404);
  });

  it('an oversize body is truncated to the cap and still accepted', async () => {
    // A body larger than the cap is truncated to MAX_BODY_BYTES and still accepted
    // (202), not rejected.
    const c = new Capture();
    const oversize = 'x'.repeat(MAX_BODY_BYTES + 100);
    const resp = await request(new Server(c.ingest).app)
      .post('/webhooks/lint')
      .set('Content-Type', 'text/plain')
      .send(oversize);

    expect(resp.status).toBe(202);
    expect(c.env!.payload.length).toBe(MAX_BODY_BYTES);
  });

  it('uses the injected clock for receivedAt', async () => {
    const c = new Capture();
    const fixed = new Date('2026-06-21T09:00:00.000Z');
    await request(new Server(c.ingest, { now: () => fixed }).app)
      .post('/webhooks/lint')
      .send('{}');

    expect(c.env!.receivedAt).toBe(fixed);
  });

  describe('verifySignature', () => {
    it('accepts a good signature and rejects bad ones', () => {
      const body = Buffer.from('{"action":"completed"}');
      const good = sign('topsecret', body.toString());

      expect(verifySignature('topsecret', good, body)).toBe(true);
      expect(verifySignature('topsecret', 'sha256=deadbeef', body)).toBe(false);
      expect(verifySignature('topsecret', '', body)).toBe(false);
      expect(verifySignature('topsecret', 'deadbeef', body)).toBe(false); // missing prefix
    });
  });
});
