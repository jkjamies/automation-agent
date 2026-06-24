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

  it('lint kickoff requires HMAC when a secret is set -> 401 without a signature', async () => {
    // A kickoff selects the caller-supplied target repo, so it is authenticated like the
    // GitHub webhook when a secret is configured.
    const c = new Capture();
    const resp = await request(new Server(c.ingest, { secret: 'topsecret' }).app)
      .post('/webhooks/lint')
      .send('{"problems":[]}');

    expect(resp.status).toBe(401);
    expect(c.env).toBeNull();
  });

  it('lint kickoff with a valid signature -> 202', async () => {
    const c = new Capture();
    const body = '{"problems":[]}';
    const resp = await request(new Server(c.ingest, { secret: 'topsecret' }).app)
      .post('/webhooks/lint')
      .set('X-Hub-Signature-256', sign('topsecret', body))
      .send(body);

    expect(resp.status).toBe(202);
    expect(c.env!.kind).toBe(Kind.Lint);
  });

  it('coverage kickoff requires HMAC when a secret is set -> 401 without a signature', async () => {
    const c = new Capture();
    const resp = await request(new Server(c.ingest, { secret: 'topsecret' }).app)
      .post('/webhooks/coverage')
      .send('{"report":"jacoco"}');

    expect(resp.status).toBe(401);
    expect(c.env).toBeNull();
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

  it('an oversize body is rejected with 413, not truncated', async () => {
    // A body larger than the cap is rejected with 413 rather than silently truncated — a
    // truncated body would both fail HMAC and feed malformed JSON downstream.
    const c = new Capture();
    const oversize = 'x'.repeat(MAX_BODY_BYTES + 100);
    const resp = await request(new Server(c.ingest).app)
      .post('/webhooks/lint')
      .set('Content-Type', 'text/plain')
      .send(oversize);

    expect(resp.status).toBe(413);
    expect(c.env).toBeNull();
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

  describe('internal endpoints', () => {
    const TOKEN = 'sekret-internal';
    const bearer = `Bearer ${TOKEN}`;

    it('404 when internal routes are disabled (no token configured)', async () => {
      const c = new Capture();
      for (const path of ['/internal/cron/daily', '/internal/cron/weekly', '/internal/sweep']) {
        const resp = await request(new Server(c.ingest).app).post(path).set('Authorization', bearer);
        expect(resp.status).toBe(404);
      }
      expect(c.env).toBeNull();
    });

    it('401 without a Bearer token', async () => {
      const c = new Capture();
      const srv = new Server(c.ingest, { internalToken: TOKEN, sweep: async () => {} }).app;
      expect((await request(srv).post('/internal/cron/daily')).status).toBe(401);
      expect((await request(srv).post('/internal/sweep').set('Authorization', 'Basic x')).status).toBe(401);
    });

    it('401 on a mismatched token', async () => {
      const c = new Capture();
      const srv = new Server(c.ingest, { internalToken: TOKEN }).app;
      const resp = await request(srv).post('/internal/cron/daily').set('Authorization', 'Bearer wrong');
      expect(resp.status).toBe(401);
      expect(c.env).toBeNull();
    });

    it('daily cron -> 202 with KindCronDaily', async () => {
      const c = new Capture();
      const resp = await request(new Server(c.ingest, { internalToken: TOKEN }).app)
        .post('/internal/cron/daily')
        .set('Authorization', bearer);
      expect(resp.status).toBe(202);
      expect(c.env!.kind).toBe(Kind.CronDaily);
      expect(c.env!.source).toBe('internal:/cron/daily');
    });

    it('weekly cron -> 202 with KindCronWeekly', async () => {
      const c = new Capture();
      const resp = await request(new Server(c.ingest, { internalToken: TOKEN }).app)
        .post('/internal/cron/weekly')
        .set('Authorization', bearer);
      expect(resp.status).toBe(202);
      expect(c.env!.kind).toBe(Kind.CronWeekly);
    });

    it('sweep -> 200 when it succeeds', async () => {
      const c = new Capture();
      let swept = false;
      const srv = new Server(c.ingest, {
        internalToken: TOKEN,
        sweep: async () => {
          swept = true;
        },
      }).app;
      const resp = await request(srv).post('/internal/sweep').set('Authorization', bearer);
      expect(resp.status).toBe(200);
      expect(swept).toBe(true);
    });

    it('sweep -> 500 when the handler throws', async () => {
      const c = new Capture();
      const srv = new Server(c.ingest, {
        internalToken: TOKEN,
        sweep: async () => {
          throw new Error('boom');
        },
      }).app;
      const resp = await request(srv).post('/internal/sweep').set('Authorization', bearer);
      expect(resp.status).toBe(500);
    });

    it('sweep -> 501 when no sweep handler is configured', async () => {
      const c = new Capture();
      const resp = await request(new Server(c.ingest, { internalToken: TOKEN }).app)
        .post('/internal/sweep')
        .set('Authorization', bearer);
      expect(resp.status).toBe(501);
    });
  });
});
