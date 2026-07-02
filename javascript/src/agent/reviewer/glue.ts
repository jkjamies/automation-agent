/**
 * The deterministic verify-gate and cross-lens merge logic the glue/synthesis pass owns.
 *
 * The glue *agent* itself (architectural alignment, testability, and test-coverage reasoning) is
 * wired in `agentsSetup` and run in `review`; cross-lens dedup and the confidence gate are done
 * here in code rather than asked of the model, so they are deterministic and unit-testable.
 */

import { type Finding, Severity, fingerprint, severityRank } from './findings';

/**
 * Remove findings below the configured minimum confidence (the phase-1 verify gate). A non-positive
 * minimum keeps everything. Never aliases the caller's array.
 */
export function dropLowConfidence(findings: Finding[], minimum: number): Finding[] {
  if (minimum <= 0) {
    return [...findings];
  }
  return findings.filter((f) => f.confidence >= minimum);
}

/**
 * Collapse findings that share a fingerprint (same file+line+message, across lenses), keeping the
 * one with the worst severity (ties broken by higher confidence). Input order is otherwise
 * preserved.
 */
export function dedupe(findings: Finding[]): Finding[] {
  const seen = new Map<string, number>(); // fingerprint -> index in out
  const out: Finding[] = [];
  for (const f of findings) {
    const fp = fingerprint(f);
    const i = seen.get(fp);
    if (i !== undefined) {
      if (better(f, out[i]!)) {
        out[i] = f;
      }
      continue;
    }
    seen.set(fp, out.length);
    out.push(f);
  }
  return out;
}

/**
 * Report whether `a` should replace `b` among duplicates: worse severity wins; on a tie, higher
 * confidence.
 */
function better(a: Finding, b: Finding): boolean {
  const ra = severityRank(a.severity);
  const rb = severityRank(b.severity);
  if (ra !== rb) {
    return ra > rb;
  }
  return a.confidence > b.confidence;
}

/**
 * Force every finding to nitpick severity. The catch-all "(other)" category is intentionally
 * low-signal, so its findings are demoted rather than allowed to drive the scorecard.
 */
export function demoteToNitpick(findings: Finding[]): Finding[] {
  for (const f of findings) {
    f.severity = Severity.Nitpick;
  }
  return findings;
}
