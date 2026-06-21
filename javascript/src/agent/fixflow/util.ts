/**
 * Text-recovery helpers for parsing model output: pull a JSON array/object out of
 * output that may add prose or code fences, and strip surrounding markdown fences from
 * generated code.
 */

/**
 * Return the substring from the first `[` to the last `]`, so a JSON array can be
 * recovered from model output that adds prose or code fences. "" if none.
 */
export function extractJsonArray(s: string): string {
  const i = s.indexOf('[');
  const j = s.lastIndexOf(']');
  if (i < 0 || j < 0 || j < i) {
    return '';
  }
  return s.slice(i, j + 1);
}

/** Return the substring from the first `{` to the last `}`. "" if none. */
export function extractJsonObject(s: string): string {
  const i = s.indexOf('{');
  const j = s.lastIndexOf('}');
  if (i < 0 || j < 0 || j < i) {
    return '';
  }
  return s.slice(i, j + 1);
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
