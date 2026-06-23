/**
 * Text-recovery helpers for parsing model output: pull a JSON array/object out of
 * output that may add prose or code fences, and strip surrounding markdown fences from
 * generated code.
 */

/**
 * Return the first complete JSON array in model output (which may add prose or code
 * fences), scanning from the first `[` — so trailing prose or a stray bracket can't
 * corrupt the span. "" if none parses.
 */
export function extractJsonArray(s: string): string {
  return firstJsonValue(s, '[', ']');
}

/** Return the first complete JSON object in model output. "" if none parses. */
export function extractJsonObject(s: string): string {
  return firstJsonValue(s, '{', '}');
}

function firstJsonValue(s: string, open: string, close: string): string {
  let start = s.indexOf(open);
  while (start >= 0) {
    const end = matchingClose(s, start, open, close);
    if (end >= 0) {
      const candidate = s.slice(start, end + 1);
      try {
        JSON.parse(candidate);
        return candidate;
      } catch {
        // balanced but not valid JSON; fall through to the next opener
      }
    }
    start = s.indexOf(open, start + 1);
  }
  return '';
}

/** Index of the `close` that balances the `open` at `start` (string-aware), or -1. */
function matchingClose(s: string, start: number, open: string, close: string): number {
  let depth = 0;
  let inStr = false;
  let escaped = false;
  for (let i = start; i < s.length; i++) {
    const c = s[i];
    if (inStr) {
      if (escaped) escaped = false;
      else if (c === '\\') escaped = true;
      else if (c === '"') inStr = false;
      continue;
    }
    if (c === '"') inStr = true;
    else if (c === open) depth++;
    else if (c === close && --depth === 0) return i;
  }
  return -1;
}

/**
 * Remove surrounding markdown code fences a model may add and normalize a trailing
 * newline.
 */
export function stripFences(out: string): string {
  let s = out.trim();
  if (s.startsWith('```')) {
    const nl = s.indexOf('\n');
    if (nl >= 0) {
      s = s.slice(nl + 1);
    }
    const j = s.lastIndexOf('```');
    if (j >= 0) {
      s = s.slice(0, j);
    }
  }
  return s.replace(/\n+$/, '') + '\n';
}
