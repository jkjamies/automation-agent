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

/** A cloned working tree. */
export class Repo {
  private readonly git: SimpleGit;
  private readonly workdir: string;

  private constructor(git: SimpleGit, dir: string) {
    this.git = git;
    this.workdir = dir;
  }

  /**
   * Clone url into dir (which must not already exist). A non-empty token is
   * embedded as GitHub HTTP auth for https URLs.
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
