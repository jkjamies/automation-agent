/**
 * Working-tree git operations the lint-fixer needs: clone, branch, stage-all,
 * commit, push — via simple-git.
 *
 * Deterministic tooling — no agent imports.
 *
 * Operations return the value and THROW on error. A clean working tree raises the
 * sentinel {@link NoChangesError}, so callers can distinguish "nothing to do" from
 * a real failure. The operations are wrapped in async Promises.
 */

import { rmSync } from 'node:fs';
import path from 'node:path';
import { simpleGit, type SimpleGit } from 'simple-git';

/** Identifies the committer. */
export interface Author {
  name: string;
  email: string;
}

/**
 * Yields a valid GitHub token for a repo (`"owner/name"`), re-fetched per git op. The
 * gitrepo-local view of `auth.TokenProvider` (a narrow interface kept here so gitrepo stays
 * decoupled from the `auth` package; the real providers match it structurally).
 */
export interface TokenProvider {
  token(repo: string): Promise<string>;
}

/**
 * Credentials clone/push use. Which one applies is chosen by the clone URL scheme, not by
 * the caller: an https remote uses `provider` (GitHub `x-access-token` basic auth, fetched
 * fresh per op so a short-lived installation token stays current), an ssh remote
 * (`git@…` / `ssh://…`) carries no in-URL credential — system git authenticates it via the
 * ambient `GIT_SSH_COMMAND` / ssh-agent / default keys.
 */
export interface Auth {
  // provider yields the token embedded as x-access-token basic auth on https remotes,
  // fetched fresh per git op (scoped to `repo`). Null/absent — or a token of "" — means
  // anonymous (public read only). Ignored for ssh remotes.
  provider?: TokenProvider | null;
  // repo is "owner/name", passed to provider so App mode can scope the token.
  repo?: string;
}

/**
 * Thrown by {@link Repo.commitAll} when the working tree is clean (the edits
 * produced no actual change), so callers can distinguish "nothing to do" from a
 * real failure.
 */
export class NoChangesError extends Error {
  constructor(message = 'gitrepo: no changes to commit') {
    super(message);
    this.name = 'NoChangesError';
  }
}

/**
 * Embed `x-access-token:<token>@` into https URLs for token auth. Non-https (local
 * path/file) remotes used in tests are returned unchanged, as is an empty token.
 */
export function authUrl(url: string, token: string): string {
  if (!token) {
    return url;
  }
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return url;
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return url;
  }
  parsed.username = 'x-access-token';
  parsed.password = token;
  return parsed.toString();
}

/**
 * Resolve the token for an https git op, fetched fresh per op so a short-lived installation
 * token stays current. Returns `''` (anonymous) for ssh / local / file remotes, which need
 * no token — fetching one would needlessly mint a GitHub installation token in App mode.
 *
 * @throws Error for a plaintext `http://` remote — sending a PAT/App token as basic auth
 *   over an unencrypted transport would leak it; use https or ssh.
 */
export async function tokenFor(url: string, auth: Auth): Promise<string> {
  if (isSshUrl(url)) {
    return '';
  }
  if (url.startsWith('http://')) {
    throw new Error('refusing to send GitHub token over insecure http remote; use https or ssh');
  }
  if (!url.startsWith('https://')) {
    return ''; // local path / file:// — no credentials.
  }
  if (!auth.provider) {
    return '';
  }
  return auth.provider.token(auth.repo ?? '');
}

/** Whether url is an scp-style (`git@host:path`) or `ssh://` remote, as opposed to https. */
function isSshUrl(url: string): boolean {
  return url.startsWith('ssh://') || url.startsWith('git@');
}

/**
 * Build the `GIT_SSH_COMMAND` value that pins git's ssh transport to an explicit private
 * key (`GIT_SSH_KEY`). `IdentitiesOnly=yes` stops ssh from also offering agent/default
 * keys, so the chosen key is the one used. Returns '' when no key is set — git then uses
 * the ssh binary's own resolution (ssh-agent, default identity files, `known_hosts`),
 * which is exactly what an https clone never touches.
 *
 * The composition root exports this as the `GIT_SSH_COMMAND` environment variable (rather
 * than simple-git's per-call `.env()`, which would *replace* the child environment and drop
 * `PATH`/`HOME`, breaking git's lookup of the `ssh` binary). Inheriting it via the process
 * environment keeps the full environment intact and obeys the "only config reads env"
 * boundary — this layer never reads the ambient environment directly.
 *
 * git word-splits `GIT_SSH_COMMAND` like a shell, so the key path is single-quoted (with any
 * embedded single quotes escaped) — a path with spaces or shell metacharacters is passed
 * through literally, never re-interpreted.
 */
export function sshCommand(sshKey: string): string {
  if (!sshKey) {
    return '';
  }
  return `ssh -i ${shellQuote(sshKey)} -o IdentitiesOnly=yes`;
}

/** POSIX single-quote a value so a shell (git parses GIT_SSH_COMMAND as one) treats it
 * literally: wrap in single quotes and replace each embedded `'` with `'\''`. */
function shellQuote(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/** A cloned working tree. */
export class Repo {
  private readonly git: SimpleGit;
  private readonly workdir: string;
  // The clean clone URL (no embedded credential) and auth are kept so push() can re-resolve
  // a fresh token per op — GitHub App installation tokens are short-lived (~1h), so a token
  // captured at clone time may be stale by push.
  private readonly url: string;
  private readonly auth: Auth;

  private constructor(git: SimpleGit, dir: string, url: string, auth: Auth) {
    this.git = git;
    this.workdir = dir;
    this.url = url;
    this.auth = auth;
  }

  /**
   * Clone url into dir (which must not already exist). Auth is chosen by the URL scheme,
   * not the caller: for an https URL a token from `auth.provider` is embedded as GitHub
   * HTTP basic auth; an ssh URL (`git@…`/`ssh://…`) passes through untouched to the system
   * git, which uses ssh-agent, the default identity files and `known_hosts` (and the
   * ambient `GIT_SSH_COMMAND` when `GIT_SSH_KEY` pins an explicit key — see
   * {@link sshCommand}). A plaintext `http://` remote is refused (token leak — see
   * {@link tokenFor}).
   *
   * @throws Error if the clone fails or an http:// remote is refused.
   */
  static async clone(url: string, dir: string, auth: Auth = {}): Promise<Repo> {
    const token = await tokenFor(url, auth);
    const cloneUrl = authUrl(url, token);
    try {
      await simpleGit().clone(cloneUrl, dir);
    } catch (err) {
      throw new Error(`clone ${url}: ${errMsg(err)}`);
    }
    const git = simpleGit(dir);
    // Don't persist the credential: git records the clone URL (which carries the token) as
    // the origin remote in .git/config, so reset it to the clean URL. The token is re-applied
    // only for the network op in push() and stripped again, so it never lingers on disk —
    // matching the Go reference, which supplies the token as transport auth, not in the URL.
    if (token) {
      try {
        await git.remote(['set-url', 'origin', url]);
      } catch (err) {
        // A failed reset would leave the token in origin — delete the partial checkout so the
        // credential can't survive on disk, then surface the failure (don't depend on the
        // caller's temp-dir cleanup to scrub it).
        let cleanupErr: unknown;
        try {
          rmSync(dir, { recursive: true, force: true });
        } catch (rmErr) {
          cleanupErr = rmErr;
        }
        throw new Error(
          cleanupErr
            ? `clone ${url}: reset remote url: ${errMsg(err)}; additionally failed to delete checkout: ${errMsg(cleanupErr)}`
            : `clone ${url}: reset remote url: ${errMsg(err)}`,
        );
      }
    }
    return new Repo(git, dir, url, auth);
  }

  /** Return the working-tree directory; callers write file edits under it. */
  dir(): string {
    return this.workdir;
  }

  /** Join rel onto the working-tree directory. */
  path(rel: string): string {
    return path.join(this.workdir, rel);
  }

  /**
   * Switch to branch, creating it from the current HEAD when create is true.
   *
   * @throws Error if the checkout fails.
   */
  async checkout(branch: string, create = false): Promise<void> {
    try {
      if (create) {
        await this.git.checkoutLocalBranch(branch);
      } else {
        await this.git.checkout(branch);
      }
    } catch (err) {
      throw new Error(`checkout ${branch}: ${errMsg(err)}`);
    }
  }

  /**
   * Check out an existing remote branch (origin/<branch>) as a local branch —
   * used on retry iterations to add a commit onto the previous fix rather than
   * starting a new branch from the base.
   *
   * @throws Error if origin/<branch> cannot be resolved or the checkout fails.
   */
  async checkoutRemote(branch: string): Promise<void> {
    let hash: string;
    try {
      hash = (await this.git.revparse([`origin/${branch}`])).trim();
    } catch (err) {
      throw new Error(`resolve origin/${branch}: ${errMsg(err)}`);
    }
    try {
      // Create a local branch at the remote hash and check it out (no -t tracking
      // dance needed).
      await this.git.checkout(['-b', branch, hash]);
    } catch (err) {
      throw new Error(`checkout ${branch}: ${errMsg(err)}`);
    }
  }

  /**
   * Stage every change (including deletions) and commit, returning the new
   * commit SHA.
   *
   * Invariant: one commit per attempt.
   *
   * @throws {NoChangesError} if the tree is clean.
   * @throws Error on a staging or commit failure.
   */
  async commitAll(msg: string, author: Author): Promise<string> {
    try {
      await this.git.add(['--all']);
    } catch (err) {
      throw new Error(`stage changes: ${errMsg(err)}`);
    }
    const status = await this.git.status();
    if (status.isClean()) {
      throw new NoChangesError();
    }
    try {
      // git defaults both the author and commit timestamps to the current time.
      // The committer identity is supplied inline via -c so the commit succeeds
      // even without a globally configured user; --author records the requested
      // author.
      await this.git.raw([
        '-c',
        `user.name=${author.name}`,
        '-c',
        `user.email=${author.email}`,
        'commit',
        '-m',
        msg,
        `--author=${author.name} <${author.email}>`,
      ]);
    } catch (err) {
      throw new Error(`commit: ${errMsg(err)}`);
    }
    // simple-git's CommitResult.commit is an abbreviated SHA; resolve the full
    // hash.
    return this.head();
  }

  /**
   * Push the current branch to origin. An up-to-date push is not an error. Credentials are
   * re-resolved here (not reused from clone) so a fresh, repo-scoped token authenticates the
   * push even if the clone-time token has since expired — for an https remote the origin URL
   * is re-pointed at the freshly-tokened form; an ssh / local remote carries no in-URL
   * credential.
   *
   * @throws Error if the push fails.
   */
  async push(): Promise<void> {
    let branch: string;
    try {
      branch = (await this.git.revparse(['--abbrev-ref', 'HEAD'])).trim();
    } catch (err) {
      throw new Error(`push: ${errMsg(err)}`);
    }
    const token = await tokenFor(this.url, this.auth);
    let pushErr: unknown;
    let resetErr: unknown;
    try {
      if (token) {
        // Apply the freshly-resolved token only for the network op.
        await this.git.remote(['set-url', 'origin', authUrl(this.url, token)]);
      }
      await this.git.push('origin', `${branch}:${branch}`);
    } catch (err) {
      pushErr = err;
    } finally {
      // Strip the token back out of .git/config so it never lingers on disk (the clone-time
      // origin URL is already clean; only push re-tokenizes it transiently).
      if (token) {
        try {
          await this.git.remote(['set-url', 'origin', this.url]);
        } catch (err) {
          // A failed reset would leave the token in .git/config — capture it and surface it
          // below rather than depend on the caller's temp-dir cleanup to scrub the credential.
          resetErr = err;
        }
      }
    }
    if (resetErr) {
      const resetMsg = `reset remote url: ${errMsg(resetErr)}`;
      throw new Error(
        pushErr ? `push: ${errMsg(pushErr)}; additionally ${resetMsg}` : `push: ${resetMsg}`,
      );
    }
    if (pushErr) {
      throw new Error(`push: ${errMsg(pushErr)}`);
    }
  }

  /**
   * Return the current HEAD commit SHA.
   *
   * @throws Error if HEAD cannot be resolved.
   */
  async head(): Promise<string> {
    try {
      return (await this.git.revparse(['HEAD'])).trim();
    } catch (err) {
      throw new Error(`head: ${errMsg(err)}`);
    }
  }
}

/** Render an unknown thrown value as a message string. */
function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
