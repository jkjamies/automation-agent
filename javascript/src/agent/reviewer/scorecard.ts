/**
 * The count-based scorecard: a per-dimension severity histogram + overall grade.
 *
 * Count-based, not a synthetic 0–100 score. The overall grade is the critical-cap (any critical in
 * an always-on critical dimension → red) combined with the worst dimension level.
 */

import { CRITICAL_DIMENSIONS, type Dimension, type Finding, Severity } from './findings';

/** A per-dimension and overall grade. Ordered so "worst level wins". */
export const Level = {
  Green: 0,
  Yellow: 1,
  Red: 2,
} as const;
export type Level = (typeof Level)[keyof typeof Level];

/** Render a level as its scorecard glyph. */
export function levelGlyph(l: Level): string {
  if (l === Level.Red) {
    return '🔴';
  }
  if (l === Level.Yellow) {
    return '🟡';
  }
  return '🟢';
}

/** The textual grade shown beside the glyph in headers and the check. */
export function levelWord(l: Level): string {
  if (l === Level.Red) {
    return 'Red';
  }
  if (l === Level.Yellow) {
    return 'Yellow';
  }
  return 'Green';
}

/** The severity histogram for one dimension plus its derived level. */
export interface DimScore {
  dimension: Dimension;
  critical: number;
  major: number;
  medium: number;
  nitpick: number;
  level: Level;
}

/** The count-based review result: a per-dimension histogram and an overall grade. */
export interface Scorecard {
  dims: DimScore[]; // sorted by dimension for stable rendering
  overall: Level;
  total: number; // total findings counted (after the confidence gate)
}

/**
 * Derive a dimension's level from its severity counts (pilot-tunable thresholds): red on any
 * critical or ≥2 major; yellow on any major or ≥3 medium; else green.
 */
export function dimLevel(s: DimScore): Level {
  if (s.critical >= 1 || s.major >= 2) {
    return Level.Red;
  }
  if (s.major >= 1 || s.medium >= 3) {
    return Level.Yellow;
  }
  return Level.Green;
}

/**
 * Build the scorecard from already-confidence-gated findings: a per-dimension histogram + level,
 * then overall = critical-cap (any critical in an always-on critical dimension → red) combined with
 * the worst dimension level.
 */
export function scoreFindings(findings: Finding[]): Scorecard {
  const byDim = new Map<Dimension, DimScore>();
  let criticalCap = false;
  for (const f of findings) {
    let d = byDim.get(f.dimension);
    if (!d) {
      d = { dimension: f.dimension, critical: 0, major: 0, medium: 0, nitpick: 0, level: Level.Green };
      byDim.set(f.dimension, d);
    }
    if (f.severity === Severity.Critical) {
      d.critical++;
      if (CRITICAL_DIMENSIONS.has(f.dimension)) {
        criticalCap = true;
      }
    } else if (f.severity === Severity.Major) {
      d.major++;
    } else if (f.severity === Severity.Medium) {
      d.medium++;
    } else {
      d.nitpick++;
    }
  }

  const card: Scorecard = { dims: [], overall: Level.Green, total: findings.length };
  let worst: Level = Level.Green;
  for (const d of byDim.values()) {
    d.level = dimLevel(d);
    if (d.level > worst) {
      worst = d.level;
    }
    card.dims.push(d);
  }
  card.dims.sort((a, b) => (a.dimension < b.dimension ? -1 : a.dimension > b.dimension ? 1 : 0));

  card.overall = criticalCap ? Level.Red : worst;
  return card;
}
