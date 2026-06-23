/**
 * Path-safe checkout file access.
 *
 * {@link safeJoin} REJECTS (not clamps) absolute paths and any path escaping the
 * checkout root via `..`. Both reads and writes route through it, so LLM-controlled
 * paths cannot touch host files.
 */
import { lstatSync, readFileSync, realpathSync } from 'node:fs';
import { basename, dirname, isAbsolute, join, normalize, sep } from 'node:path';

/**
 * Resolve a repo-relative path against `root`, throwing on absolute paths or paths
 * that escape the root via `..`.
 *
 * @throws Error for an absolute or escaping path.
 */
export function safeJoin(root: string, rel: string): string {
  if (isAbsolute(rel)) {
    throw new Error(`absolute path ${JSON.stringify(rel)} not allowed`);
  }
  const full = normalize(join(root, rel));
  if (full !== root && !full.startsWith(root + sep)) {
    throw new Error(`path ${JSON.stringify(rel)} escapes the repo`);
  }
  // Symlink containment: a symlinked directory inside the (attacker-influenced) checkout could
  // redirect an in-bounds path outside the root, so re-check the real location. realpathSync
  // throws on a not-yet-created target, so resolve the deepest existing ancestor; both sides
  // are resolved so a symlinked temp root doesn't false-reject.
  const rootReal = realpathSync(root);
  const fullReal = resolveExisting(full);
  if (fullReal === null || (fullReal !== rootReal && !fullReal.startsWith(rootReal + sep))) {
    throw new Error(`path ${JSON.stringify(rel)} escapes the repo via a symlink`);
  }
  return full;
}

/** Whether the entry at `p` exists without following a final symlink. */
function entryExists(p: string): boolean {
  try {
    lstatSync(p);
    return true;
  } catch {
    return false;
  }
}

/**
 * Return `p` with its longest existing ancestor symlink-resolved and any not-yet-created
 * remainder appended lexically, or null if a component exists but cannot be resolved (a
 * dangling or looping symlink), which could redirect a write outside the root.
 */
function resolveExisting(p: string): string | null {
  const rest: string[] = [];
  let cur = p;
  for (;;) {
    try {
      const real = realpathSync(cur);
      return rest.length === 0 ? real : join(real, ...rest);
    } catch {
      // realpathSync failed. If the entry itself exists (lstat doesn't follow the final
      // link), it's a dangling/looping symlink — reject. Otherwise it's truly missing.
      if (entryExists(cur)) {
        return null;
      }
      const parent = dirname(cur);
      if (parent === cur) {
        return p; // reached the filesystem root; nothing resolved
      }
      rest.unshift(basename(cur));
      cur = parent;
    }
  }
}

/** Read a repo-relative file from the checkout (path-safe). */
export function readFile(root: string, rel: string): string {
  return readFileSync(safeJoin(root, rel), 'utf-8');
}
