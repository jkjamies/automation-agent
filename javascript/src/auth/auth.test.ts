/**
 * Tests for the auth seam (TokenProvider).
 *
 * StaticProvider needs no network. AppProvider is exercised against a localhost stub of the
 * GitHub installation token-exchange endpoint (the analog of the Go reference's `httptest`
 * stub): a throwaway RSA key signs the App JWT, and the stub captures it to assert RS256 /
 * issuer / the pinned-installation path, plus caching and refresh. No live network, no LLM.
 */
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { generateKeyPairSync } from 'node:crypto';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProvider, StaticProvider, newAppProvider } from './auth';

function rsaPem(): string {
  const { privateKey } = generateKeyPairSync('rsa', {
    modulusLength: 2048,
    publicKeyEncoding: { type: 'spki', format: 'pem' },
    privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
  });
  return privateKey;
}

// One throwaway key shared across cases (RSA keygen is slow).
const PEM = rsaPem();
const FAR_FUTURE = '2099-01-01T00:00:00Z';
const TOKEN = 'ghs_installation_token';

/** Decode a JWT's header + payload (base64url JSON) without verifying the signature. */
function decodeJwt(jwt: string): { header: Record<string, unknown>; payload: Record<string, unknown> } {
  const [h, p] = jwt.split('.');
  const header = JSON.parse(Buffer.from(h!, 'base64url').toString('utf-8'));
  const payload = JSON.parse(Buffer.from(p!, 'base64url').toString('utf-8'));
  return { header, payload };
}

/**
 * A localhost stub of POST /app/installations/{id}/access_tokens. Captures each request's
 * path + Authorization (the App JWT) and returns a fixed installation token with a
 * configurable `expiresAt` (mutate before a call to drive cache vs refresh).
 */
class Stub {
  expiresAt = FAR_FUTURE;
  slug = 'octoapp';
  requests: Array<{ path: string; auth: string }> = [];
  private readonly server: Server;

  constructor() {
    this.server = createServer((req, res) => {
      let body = '';
      req.on('data', (chunk) => {
        body += chunk;
      });
      req.on('end', () => {
        void body;
        const path = req.url ?? '';
        this.requests.push({ path, auth: req.headers.authorization ?? '' });
        // GET /app resolves the App's own identity (its slug) via the App JWT.
        if (path === '/app') {
          res.writeHead(200, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ slug: this.slug }));
          return;
        }
        const payload = JSON.stringify({
          token: TOKEN,
          expires_at: this.expiresAt,
          permissions: {},
          repository_selection: 'all',
        });
        res.writeHead(201, { 'Content-Type': 'application/json' });
        res.end(payload);
      });
    });
  }

  get appRequests(): Array<{ path: string; auth: string }> {
    return this.requests.filter((r) => r.path === '/app');
  }

  listen(): Promise<void> {
    return new Promise((resolve) => this.server.listen(0, '127.0.0.1', () => resolve()));
  }

  close(): Promise<void> {
    return new Promise((resolve) => this.server.close(() => resolve()));
  }

  get baseUrl(): string {
    const addr = this.server.address() as AddressInfo;
    return `http://127.0.0.1:${addr.port}`;
  }

  get tokenRequests(): Array<{ path: string; auth: string }> {
    return this.requests.filter((r) => r.path.includes('access_tokens'));
  }
}

// --- StaticProvider (no network) --------------------------------------------

describe('StaticProvider', () => {
  it('returns a constant token for every repo and caches one client', async () => {
    const p = new StaticProvider('pat-123');
    expect(await p.token('acme/api')).toBe('pat-123');
    expect(await p.token('other/repo')).toBe('pat-123'); // repo ignored
    expect(p.github()).toBe(p.github()); // one cached client
  });

  it('an empty token is anonymous (unauthenticated client, no crash)', async () => {
    const p = new StaticProvider('');
    expect(await p.token('acme/api')).toBe('');
    expect(p.github()).toBeDefined();
  });

  it('authoredLogin resolves the PAT user login, but is "" for an empty token', async () => {
    expect(await new StaticProvider('').authoredLogin()).toBe(''); // anonymous, no identity

    const p = new StaticProvider('pat-123');
    // Redirect the private client at a fake GET /user without a real network call.
    (p as unknown as { gh: { rest: { users: { getAuthenticated: () => Promise<{ data: { login: string } }> } } } }).gh = {
      rest: { users: { getAuthenticated: () => Promise.resolve({ data: { login: 'pat-user' } }) } },
    };
    expect(await p.authoredLogin()).toBe('pat-user');
  });
});

// --- AppProvider (localhost token-exchange stub) ----------------------------

describe('AppProvider', () => {
  let stub: Stub;

  beforeEach(async () => {
    stub = new Stub();
    await stub.listen();
  });

  afterEach(async () => {
    await stub.close();
  });

  it('mints a token against the pinned installation, signing an RS256 App JWT', async () => {
    const p = newAppProvider(42, 99, PEM, stub.baseUrl);
    expect(p).toBeInstanceOf(AppProvider);

    expect(await p.token('acme/api')).toBe(TOKEN);

    expect(stub.tokenRequests).toHaveLength(1);
    const { path, auth } = stub.tokenRequests[0]!;
    // Pinned single installation (Decision §1): the exchange targets installation 99.
    expect(path).toMatch(/\/app\/installations\/99\/access_tokens$/);
    // The request authenticates as the App with an RS256 JWT issued by the app id
    // (Octokit emits the `bearer` scheme lower-case on the token-exchange call).
    expect(auth).toMatch(/^bearer /i);
    const { header, payload } = decodeJwt(auth.replace(/^bearer /i, ''));
    expect(header.alg).toBe('RS256');
    expect(String(payload.iss)).toBe('42');
  });

  it('caches a still-valid token (one exchange across reads)', async () => {
    stub.expiresAt = FAR_FUTURE;
    const p = newAppProvider(42, 99, PEM, stub.baseUrl);
    expect(await p.token('acme/api')).toBe(TOKEN);
    expect(await p.token('acme/api')).toBe(TOKEN);
    expect(stub.tokenRequests).toHaveLength(1);
  });

  it('refreshes once the cached token nears expiry (TTL eviction)', async () => {
    // auth-app caches the installation token for ~59m (one minute inside the 1h GitHub
    // expiry), then re-mints. Fake only `Date` so the cache's expiry check trips on the
    // advanced clock while the real network call to the stub still works.
    vi.useFakeTimers({ toFake: ['Date'] });
    try {
      const p = newAppProvider(42, 99, PEM, stub.baseUrl);
      await p.token('acme/api');
      expect(stub.tokenRequests).toHaveLength(1);
      // Jump past the ~59m cache TTL: the next read must re-exchange.
      vi.setSystemTime(Date.now() + 60 * 60 * 1000);
      await p.token('acme/api');
      expect(stub.tokenRequests).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('shares one Octokit client between REST and git', () => {
    const p = newAppProvider(42, 99, PEM, stub.baseUrl);
    expect(p.github()).toBe(p.github());
  });

  it('resolves and caches the "<slug>[bot]" authored login via a JWT GET /app', async () => {
    const p = newAppProvider(42, 99, PEM, stub.baseUrl);
    expect(await p.authoredLogin()).toBe('octoapp[bot]');
    expect(await p.authoredLogin()).toBe('octoapp[bot]'); // cached — no second lookup
    expect(stub.appRequests).toHaveLength(1);
    // GET /app authenticates as the App with an RS256 JWT (bearer), not the installation token.
    expect(stub.appRequests[0]!.auth).toMatch(/^bearer /i);
  });

  it('throws when the app identity response has no slug', async () => {
    stub.slug = '';
    const p = newAppProvider(42, 99, PEM, stub.baseUrl);
    await expect(p.authoredLogin()).rejects.toThrow(/no slug/);
  });
});
