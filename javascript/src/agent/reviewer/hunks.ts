/**
 * Which head-side lines of a patch GitHub accepts an inline comment on, used to route a finding to
 * an inline comment vs. the summary's out-of-diff section.
 */

import type { PRFile } from '../../githubapi/client';

/**
 * Return the new-side (head) line numbers in a unified-diff patch that GitHub will accept a
 * RIGHT-side inline comment on: added ('+') and context (' ') lines. Removed ('-') lines have no
 * head-side line and are skipped. A malformed or empty patch yields an empty set, so a finding on
 * it is treated as out-of-diff rather than posted at a wrong line.
 */
export function commentableLines(patch: string): Set<number> {
  const out = new Set<number>();
  let newLine = 0;
  let inHunk = false;
  for (const line of patch.split('\n')) {
    if (line.startsWith('@@')) {
      const parsed = parseHunkNewStart(line);
      newLine = parsed.start;
      inHunk = parsed.ok;
      continue;
    }
    if (!inHunk) {
      continue;
    }
    if (line.startsWith('+')) {
      out.add(newLine);
      newLine += 1;
    } else if (line.startsWith('-')) {
      // removed line: advances the old side only, no head-side line
    } else if (line.startsWith(' ')) {
      out.add(newLine);
      newLine += 1;
    } else if (line.startsWith('\\')) {
      // "\ No newline at end of file": metadata, not a line
    } else {
      // a blank or unexpected line ends this hunk's body
      inHunk = false;
    }
  }
  return out;
}

/**
 * Parse the new-file starting line from a hunk header "@@ -a,b +c,d @@", returning `{ start: c, ok:
 * true }`. A header it cannot parse yields `{ start: 0, ok: false }` so the body until the next
 * header is skipped rather than mis-numbered.
 */
function parseHunkNewStart(header: string): { start: number; ok: boolean } {
  const plus = header.indexOf('+');
  if (plus < 0) {
    return { start: 0, ok: false };
  }
  let rest = header.slice(plus + 1);
  const end = indexAny(rest, ' ,');
  if (end >= 0) {
    rest = rest.slice(0, end);
  }
  if (!/^\d+$/.test(rest)) {
    return { start: 0, ok: false };
  }
  const n = Number.parseInt(rest, 10);
  if (n <= 0) {
    return { start: 0, ok: false };
  }
  return { start: n, ok: true };
}

/** Return the index of the first character in `s` that is in `chars`, or -1. */
function indexAny(s: string, chars: string): number {
  for (let i = 0; i < s.length; i++) {
    if (chars.includes(s[i]!)) {
      return i;
    }
  }
  return -1;
}

/** Maps each changed file to the head-side lines an inline comment can target. */
export class DiffIndex {
  private readonly idx = new Map<string, Set<number>>();

  /** Build the in-diff line index for a set of changed files. */
  constructor(files: PRFile[]) {
    for (const f of files) {
      this.idx.set(f.path, commentableLines(f.patch));
    }
  }

  /** Report whether file:line falls on a commentable head-side line of the diff. */
  inDiff(file: string, line: number): boolean {
    const lines = this.idx.get(file);
    return lines !== undefined && lines.has(line);
  }
}
