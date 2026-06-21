/**
 * Path-safe checkout file access.
 *
 * {@link safeJoin} REJECTS (not clamps) absolute paths and any path escaping the
 * checkout root via `..`. Both reads and writes route through it, so LLM-controlled
 * paths cannot touch host files.
 */
import { readFileSync } from 'node:fs';
import { isAbsolute, normalize, join, sep } from 'node:path';

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
  return full;
}

/** Read a repo-relative file from the checkout (path-safe). */
export function readFile(root: string, rel: string): string {
  return readFileSync(safeJoin(root, rel), 'utf-8');
}
