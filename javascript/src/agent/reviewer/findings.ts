/**
 * The finding schema, severity/dimension normalization, fingerprint, and the defensive
 * {@link parseFindings}.
 *
 * A category agent emits a JSON array of findings; local models wrap that JSON in prose or ```
 * fences and occasionally emit nothing, so parsing is best-effort by design — it pulls the first
 * decodable JSON array out of the text and treats a malformed body as no findings (empty =
 * success). The narrow single-lens prompts are themselves the false-positive control.
 */

/**
 * Ranks a finding's importance. critical/major/medium are actionable (posted inline); nitpick is
 * collapsed/low-noise.
 */
export const Severity = {
  Critical: 'critical',
  Major: 'major',
  Medium: 'medium',
  Nitpick: 'nitpick',
} as const;
export type Severity = (typeof Severity)[keyof typeof Severity];

/** Order severities (higher = worse) so dedup can keep the worst of a pair. */
export function severityRank(s: Severity): number {
  switch (s) {
    case Severity.Critical:
      return 4;
    case Severity.Major:
      return 3;
    case Severity.Medium:
      return 2;
    case Severity.Nitpick:
      return 1;
    default:
      return 0;
  }
}

/**
 * Map a model-emitted severity onto a known value, defaulting an unknown or blank value to
 * nitpick — the safe, low-noise bucket (a local model is biased toward fewer-but-real).
 */
export function normalizeSeverity(s: string): Severity {
  const v = s.trim().toLowerCase();
  if (v === Severity.Critical) {
    return Severity.Critical;
  }
  if (v === Severity.Major) {
    return Severity.Major;
  }
  if (v === Severity.Medium) {
    return Severity.Medium;
  }
  return Severity.Nitpick;
}

/**
 * One of the review lenses. A category agent tags each finding with the dimension it belongs to;
 * the scorecard is a per-dimension histogram.
 */
export const Dimension = {
  RuntimeSafety: 'runtime_safety',
  ErrorHandling: 'error_handling',
  Security: 'security',
  Performance: 'performance',
  PatternViolation: 'pattern_violation',
  Maintainability: 'maintainability',
  Readability: 'readability',
  Documentation: 'documentation',
  Accessibility: 'accessibility',
  Architecture: 'architectural_alignment',
  Testability: 'testability',
  TestCoverage: 'test_coverage',
  Other: 'other',
} as const;
export type Dimension = (typeof Dimension)[keyof typeof Dimension];

const KNOWN_DIMENSIONS: ReadonlySet<string> = new Set(Object.values(Dimension));

/**
 * Map a model-emitted dimension onto a known value (lowercased, spaces and hyphens folded to
 * underscores), defaulting an unrecognized one to `other`.
 */
export function normalizeDimension(s: string): Dimension {
  const d = s.trim().toLowerCase().replaceAll(' ', '_').replaceAll('-', '_');
  if (KNOWN_DIMENSIONS.has(d)) {
    return d as Dimension;
  }
  return Dimension.Other;
}

/**
 * The always-on dimensions where a critical finding caps the overall grade to red regardless of
 * the other lenses.
 */
export const CRITICAL_DIMENSIONS: ReadonlySet<Dimension> = new Set<Dimension>([
  Dimension.Security,
  Dimension.RuntimeSafety,
]);

/** One review observation from a category agent or the glue pass. */
export interface Finding {
  file: string;
  line: number;
  dimension: Dimension;
  severity: Severity;
  message: string;
  suggestion: string; // optional ```suggestion body (a localized in-diff fix)
  fixPrompt: string; // optional "Prompt for AI agents" body (feeds the future fix hand-off)
  ruleId: string; // optional repo-standard rule id this finding cites
  confidence: number; // 0..1; below REVIEW_MIN_CONFIDENCE is dropped before scoring
}

/** Build a Finding, defaulting every unset field to its zero value. */
export function newFinding(partial: Partial<Finding> = {}): Finding {
  return {
    file: '',
    line: 0,
    dimension: Dimension.Other,
    severity: Severity.Nitpick,
    message: '',
    suggestion: '',
    fixPrompt: '',
    ruleId: '',
    confidence: 0,
    ...partial,
  };
}

/**
 * Identify a finding across re-reviews for reconciliation and for cross-lens dedup: file + line +
 * a normalized message. Dimension is deliberately omitted so the same line/message surfaced by two
 * different lenses collapses to one finding.
 */
export function fingerprint(f: Finding): string {
  return `${f.file}:${f.line}:${normalizeMessage(f.message)}`;
}

/**
 * Lowercase and collapse internal whitespace so trivially different renderings of the same message
 * fingerprint identically.
 */
export function normalizeMessage(s: string): string {
  return s.toLowerCase().split(/\s+/).filter(Boolean).join(' ');
}

// The wire keys whose values must be strings when present (a strict typed decode).
const STR_FIELDS = ['file', 'dimension', 'severity', 'message', 'suggestion', 'fix_prompt', 'rule_id'];

/**
 * Extract findings from a category agent's raw output. Best-effort by design: it pulls the first
 * JSON array out of the text and tolerates a malformed body by returning no findings (empty =
 * success). It never throws, so a garbled response degrades to "no findings for this lens" rather
 * than failing the whole review.
 */
export function parseFindings(raw: string): Finding[] {
  const wires = decodeFirstFindingArray(raw);
  if (!wires) {
    return [];
  }
  const out: Finding[] = [];
  for (const w of wires) {
    const message = str(w.message).trim();
    if (message === '') {
      continue; // a finding with no message is unusable
    }
    out.push(
      newFinding({
        file: str(w.file).trim(),
        line: numOr(w.line, 0),
        dimension: normalizeDimension(str(w.dimension)),
        severity: normalizeSeverity(str(w.severity)),
        message,
        suggestion: str(w.suggestion).trim(),
        fixPrompt: str(w.fix_prompt).trim(),
        ruleId: str(w.rule_id).trim(),
        confidence: clampConfidence(asFloat(w.confidence)),
      }),
    );
  }
  return out;
}

/**
 * Scan `raw` for the first `[` that begins a JSON array decoding cleanly into the findings shape,
 * returning its elements. Scanning for a *decodable* array (rather than slicing the first `[` to
 * the last `]`) tolerates ``` fences, prose, and stray brackets without over-grabbing. A valid but
 * empty array is skipped in case a populated one follows; if none decodes, it returns null
 * (best-effort: empty = success). Trailing prose after the array is ignored.
 */
function decodeFirstFindingArray(raw: string): Array<Record<string, unknown>> | null {
  for (let i = 0; i < raw.length; i++) {
    if (raw[i] !== '[') {
      continue;
    }
    const end = matchArrayEnd(raw, i);
    if (end < 0) {
      continue;
    }
    let value: unknown;
    try {
      value = JSON.parse(raw.slice(i, end + 1));
    } catch {
      continue;
    }
    if (Array.isArray(value) && value.length > 0 && validFindingArray(value)) {
      return value as Array<Record<string, unknown>>;
    }
  }
  return null;
}

/**
 * Return the index of the `]` that closes the `[` at `start`, respecting string literals and
 * escapes, or -1 if the array is unterminated. Mirrors a JSON decoder scanning one array value out
 * of a larger text without slicing to the last bracket.
 */
function matchArrayEnd(raw: string, start: number): number {
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = start; i < raw.length; i++) {
    const ch = raw[i];
    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (ch === '\\') {
        escaped = true;
      } else if (ch === '"') {
        inString = false;
      }
      continue;
    }
    if (ch === '"') {
      inString = true;
    } else if (ch === '[') {
      depth++;
    } else if (ch === ']') {
      depth--;
      if (depth === 0) {
        return i;
      }
    }
  }
  return -1;
}

/**
 * Report whether every element decodes cleanly into the findings shape: an object whose known
 * string fields are strings, whose `line` (if present) is an integer, and whose `confidence` (if
 * present) is a finite number. A type mismatch fails the whole array so the scan moves on to the
 * next bracket, mirroring a strict typed decode. A non-finite `confidence` (NaN/Infinity) is
 * rejected here in the validation layer.
 */
function validFindingArray(value: unknown[]): boolean {
  for (const el of value) {
    if (typeof el !== 'object' || el === null || Array.isArray(el)) {
      return false;
    }
    const obj = el as Record<string, unknown>;
    for (const key of STR_FIELDS) {
      if (key in obj && typeof obj[key] !== 'string') {
        return false;
      }
    }
    if ('line' in obj) {
      const line = obj.line;
      if (typeof line !== 'number' || !Number.isInteger(line)) {
        return false;
      }
    }
    if ('confidence' in obj) {
      const c = obj.confidence;
      if (typeof c !== 'number' || !Number.isFinite(c)) {
        return false;
      }
    }
  }
  return true;
}

/** Coerce a JSON number to a number; anything else is treated as unspecified (0). */
function asFloat(v: unknown): number {
  if (typeof v === 'boolean') {
    return 0;
  }
  if (typeof v === 'number') {
    return v;
  }
  return 0;
}

/**
 * Normalize a confidence *threshold* into [0,1]. Unlike {@link clampConfidence} (which treats 0 as
 * "unspecified"), a 0 threshold is meaningful — it disables the gate (keep all) — so NaN and
 * negatives fold to 0 (keep all, the safe default) and values above 1 fold to 1.
 */
export function clampThreshold(f: number): number {
  if (!(f >= 0)) {
    // also catches NaN
    return 0;
  }
  if (f > 1) {
    return 1;
  }
  return f;
}

/**
 * Keep confidence in [0,1]. A zero/absent value is treated as 0.5 (unspecified) so a model that
 * omits the field is not silently dropped by the gate.
 */
export function clampConfidence(c: number): number {
  if (c <= 0) {
    return 0.5;
  }
  if (c > 1) {
    return 1;
  }
  return c;
}

/** Render findings as a compact JSON array for embedding in the glue prompt. */
export function findingsJson(findings: Finding[]): string {
  return JSON.stringify(
    findings.map((f) => ({
      file: f.file,
      line: f.line,
      dimension: f.dimension,
      severity: f.severity,
      message: f.message,
      suggestion: f.suggestion,
      fix_prompt: f.fixPrompt,
      rule_id: f.ruleId,
      confidence: f.confidence,
    })),
  );
}

/** Coerce a possibly-missing string field to `""`. */
function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

/** Coerce a possibly-missing number field to `def`. */
function numOr(v: unknown, def: number): number {
  return typeof v === 'number' ? v : def;
}
