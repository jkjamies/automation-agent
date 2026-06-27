/**
 * Authentication seam: how the service authenticates to GitHub, hidden behind a single
 * interface, {@link TokenProvider}, so the rest of the code never sees whether a token
 * came from a static PAT or a freshly minted GitHub App installation token.
 *
 * Two providers implement the seam:
 *
 *   - {@link StaticProvider} — returns one constant token for every repo. Backs the PAT
 *     local-dev fallback (`GITHUB_TOKEN` / `GH_TOKEN` / `gh auth token`) and the empty,
 *     anonymous client used for public reads and tests.
 *   - {@link AppProvider} — mints and caches a short-lived (~1h), auto-refreshed
 *     installation token for a single pinned installation (single-org per deployment; see
 *     `specs/20260625-github-app-authentication.md` §1). The `repo` argument is accepted
 *     for the contract but ignored: one installation covers every repo in the deployment.
 *
 * Each provider exposes two views of the same credential, one per consumer:
 *
 *   - `token(repo)` — the raw token string the git transport embeds as `x-access-token`
 *     basic auth, re-fetched per git operation so a short-lived installation token stays
 *     current (mirrors the Go/Python references, where gitrepo fetches a token per op). It
 *     is async because the App token may need minting/refresh over the network.
 *   - `github()` — a ready Octokit REST client. {@link AppProvider} builds it with
 *     `@octokit/auth-app`'s `createAppAuth` strategy so the token auto-refreshes per
 *     request; `token()` reads the cached token off the *same* Octokit auth hook, so REST
 *     and git share one cached installation token (no double mint).
 *
 * The Go reference bridges the seam to the REST client with a token-injecting
 * RoundTripper; Octokit already owns the REST client and its auth refresh (via the
 * auth-app strategy), so the idiomatic shape here is to let the provider hand back a ready
 * client. The external contract (env vars, mode selection, App-vs-PAT behavior) is
 * identical across ports.
 *
 * Deterministic tooling — no agent imports (an arch test enforces this).
 */
import { createAppAuth } from '@octokit/auth-app';
import { Octokit } from '@octokit/rest';

/** Bounds the token-exchange and REST requests so a stalled connection can't hang startup. */
const HTTP_TIMEOUT_MS = 30_000;

/**
 * Yields a valid GitHub token / REST client for operations on a repo. PAT mode returns the
 * same constant for every repo; App mode mints/caches an installation token and refreshes
 * it before expiry. `repo` is `"owner/name"`.
 */
export interface TokenProvider {
  /** A currently-valid token; `""` means anonymous (public read only). Async: App mode may mint. */
  token(repo: string): Promise<string>;
  /** The Octokit REST client this provider authenticates. */
  github(): Octokit;
}

/**
 * Returns the same token for every repo. Backs PAT mode and the empty/anonymous client (an
 * empty token yields an unauthenticated client, fine for public reads and tests).
 */
export class StaticProvider implements TokenProvider {
  private readonly tok: string;
  private readonly gh: Octokit;

  constructor(token = '') {
    this.tok = token;
    // An empty token must build an *unauthenticated* client (Octokit's token auth rejects "").
    const opts = { request: { timeout: HTTP_TIMEOUT_MS } };
    this.gh = token ? new Octokit({ auth: token, ...opts }) : new Octokit(opts);
  }

  /** Return the constant token; `repo` is ignored. */
  token(_repo: string): Promise<string> {
    return Promise.resolve(this.tok);
  }

  github(): Octokit {
    return this.gh;
  }
}

/**
 * Mints and caches a short-lived installation token for a single pinned installation.
 * `@octokit/auth-app`'s `createAppAuth` strategy handles the App JWT (RS256) signing, the
 * token exchange, caching, and proactive refresh; {@link AppProvider} adapts it to the
 * seam. `token()` reads the installation token off the same Octokit auth hook the REST
 * client uses, so both reuse one cached installation token.
 */
export class AppProvider implements TokenProvider {
  private readonly gh: Octokit;

  constructor(appId: number, installationId: number, privateKey: string, baseUrl = '') {
    // The auth-app strategy uses this Octokit's own request (so the configured base URL is
    // honored) to POST /app/installations/{id}/access_tokens, then caches/refreshes the
    // result. baseUrl is overridden in tests to point at a local stub; production omits it
    // and uses the default https://api.github.com.
    const opts = {
      authStrategy: createAppAuth,
      auth: { appId, privateKey, installationId },
      request: { timeout: HTTP_TIMEOUT_MS },
    };
    this.gh = baseUrl ? new Octokit({ ...opts, baseUrl }) : new Octokit(opts);
  }

  /**
   * Return a currently-valid installation token, minting on first call then refreshing
   * before expiry. `repo` is ignored: the installation is pinned and covers every repo in
   * the deployment.
   */
  async token(_repo: string): Promise<string> {
    const auth = (await this.gh.auth({ type: 'installation' })) as { token: string };
    return auth.token;
  }

  github(): Octokit {
    return this.gh;
  }
}

/**
 * Build an App provider pinned to one installation. `privateKey` is the App private key in
 * PEM form — the caller sources and validates it (see `config`). `baseUrl` points the
 * token exchange at a local stub in tests; production omits it.
 */
export function newAppProvider(
  appId: number,
  installationId: number,
  privateKey: string,
  baseUrl = '',
): AppProvider {
  return new AppProvider(appId, installationId, privateKey, baseUrl);
}
