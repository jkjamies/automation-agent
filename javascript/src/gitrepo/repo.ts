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

import path from 'node:path';
import { simpleGit, type SimpleGit } from 'simple-git';

/** Identifies the committer. */
export interface Author {
  name: string;
  email: string;
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

  private constructor(git: SimpleGit, dir: string) {
    this.git = git;
    this.workdir = dir;
  }

  /**
   * Clone url into dir (which must not already exist). Auth is chosen by the URL scheme,
   * not the caller: a non-empty token is embedded as GitHub HTTP auth for https URLs;
   * an ssh URL (`git@…`/`ssh://…`) passes through {@link authUrl} untouched to the system
   * git, which uses ssh-agent, the default identity files and `known_hosts` (and the
   * ambient `GIT_SSH_COMMAND` when `GIT_SSH_KEY` pins an explicit key — see
   * {@link sshCommand}).
   *
   * @throws Error if the clone fails.
   */
  static async clone(url: string, dir: string, token = ''): Promise<Repo> {
    const cloneUrl = authUrl(url, token);
    try {
      await simpleGit().clone(cloneUrl, dir);
    } catch (err) {
      throw new Error(`clone ${url}: ${errMsg(err)}`);
    }
    return new Repo(simpleGit(dir), dir);
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
   * Push the current branch to origin. An up-to-date push is not an error.
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
    try {
      await this.git.push('origin', `${branch}:${branch}`);
    } catch (err) {
      throw new Error(`push: ${errMsg(err)}`);
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
