/**
 * The exclude-glob file filter that drops generated/vendored/binary churn and totals the filtered
 * patch bytes.
 *
 * Filtering first is the biggest cheap win: most "huge" PRs are mostly lockfile/vendor churn and
 * shrink to a handful of real files, so size is computed on the *filtered* set.
 */

import type { PRFile } from '../../githubapi/client';

/**
 * One compiled exclude glob. A pattern with no '/' matches against the file's basename (e.g.
 * "*.min.js", "go.sum"); a pattern with a '/' matches against the full path (e.g. "vendor/**").
 * "**" matches across path separators; "*" and "?" do not.
 */
export interface GlobPattern {
  re: RegExp;
  basename: boolean;
}

/**
 * Drops changed files that are not worth reviewing — generated code, vendored trees, lockfiles,
 * minified bundles, snapshots, and binaries — before any size accounting or model call.
 */
export class FileFilter {
  private readonly patterns: GlobPattern[] = [];

  /**
   * Compile the exclude globs. Blank entries (e.g. a trailing comma in the env value) are skipped.
   * Every glob compiles — {@link globToRegExp} escapes all regexp metacharacters — so this cannot
   * fail.
   */
  constructor(globs: string[]) {
    for (const raw of globs) {
      const g = raw.trim();
      if (g === '') {
        continue;
      }
      this.patterns.push({ re: globToRegExp(g), basename: !g.includes('/') });
    }
  }

  /** Report whether a path matches any exclude glob. */
  excluded(p: string): boolean {
    const base = basename(p);
    for (const pat of this.patterns) {
      const target = pat.basename ? base : p;
      if (pat.re.test(target)) {
        return true;
      }
    }
    return false;
  }

  /**
   * Return the kept (non-excluded) files and the total size of their patches in bytes. Size is
   * computed on the filtered set so the size gate sees real review surface, not churn; files whose
   * patch GitHub omitted are charged conservatively (see {@link patchBytes}) so an oversized PR
   * cannot undercount its way past the byte cap.
   */
  apply(files: PRFile[]): { kept: PRFile[]; diffBytes: number } {
    const kept: PRFile[] = [];
    let diffBytes = 0;
    for (const fl of files) {
      if (this.excluded(fl.path)) {
        continue;
      }
      kept.push(fl);
      diffBytes += patchBytes(fl);
    }
    return { kept, diffBytes };
  }
}

/**
 * The per-changed-line byte estimate charged when GitHub omits a file's patch for an oversized text
 * diff. A unified-diff line is its content plus a one-char +/- prefix and a newline; real source
 * lines average well above this, so the estimate is deliberately conservative: the size gate must
 * over-, never under-, charge an omitted diff so a very large PR cannot slip the byte cap by
 * changing files too big for GitHub to diff.
 */
export const AVG_DIFF_LINE_BYTES = 50;

/**
 * The diff-byte cost charged for one kept file. When GitHub returns the patch it is the exact byte
 * length. When GitHub omits it for an oversized text file (empty patch but non-zero line counts) it
 * is estimated from the reported additions+deletions. Binary files (no patch, no line counts) cost
 * nothing.
 */
export function patchBytes(fl: PRFile): number {
  if (fl.patch !== '') {
    return Buffer.byteLength(fl.patch, 'utf-8');
  }
  const lines = fl.additions + fl.deletions;
  if (lines > 0) {
    return lines * AVG_DIFF_LINE_BYTES;
  }
  return 0;
}

const METACHARS = new Set('.+()|[]{}^$\\'.split(''));

/**
 * Compile a glob into an anchored regexp. "**" becomes ".*" (crosses path separators), "*" becomes
 * "[^/]*" and "?" becomes "[^/]" (within one segment); every other regexp metacharacter is escaped
 * so it matches literally. Because all metacharacters are either escaped or rewritten, the result
 * is always a valid pattern.
 */
export function globToRegExp(glob: string): RegExp {
  const b: string[] = ['^'];
  const n = glob.length;
  let i = 0;
  while (i < n) {
    const c = glob[i]!;
    if (c === '*') {
      if (i + 1 < n && glob[i + 1] === '*') {
        b.push('.*');
        i++; // consume the second '*'
      } else {
        b.push('[^/]*');
      }
    } else if (c === '?') {
      b.push('[^/]');
    } else if (METACHARS.has(c)) {
      b.push('\\', c);
    } else {
      b.push(c);
    }
    i++;
  }
  b.push('$');
  return new RegExp(b.join(''));
}

/** The final path segment (basename), splitting on '/' as posix paths do. */
function basename(p: string): string {
  const idx = p.lastIndexOf('/');
  return idx < 0 ? p : p.slice(idx + 1);
}
