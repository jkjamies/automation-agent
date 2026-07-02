/**
 * The hidden fingerprint marker and the pure reconcile: given this run's inline findings + the PR's
 * existing comments, what to post vs minimize.
 *
 * The marker tags each inline comment with the fingerprint of the finding that produced it, so a
 * later re-review re-identifies the comment from GitHub itself (GitHub-as-store — no local durable
 * state). It is an HTML comment appended to the body and is an external-ish contract: keep the exact
 * format stable across ports.
 */

import type { ReviewCommentRef } from '../../githubapi/client';
import { type Finding, fingerprint } from './findings';

const FP_MARKER_PREFIX = '<!-- ar-fp:';
const FP_MARKER_SUFFIX = ' -->';

// Extracts the fingerprint from a comment body. Non-greedy so a body with trailing content still
// matches only the marker payload.
const FP_MARKER_PATTERN = /<!-- ar-fp:(.+?) -->/;

/** Render the hidden fingerprint marker appended to an inline comment body. */
export function fpMarker(fp: string): string {
  return FP_MARKER_PREFIX + fp + FP_MARKER_SUFFIX;
}

/**
 * Return the fingerprint embedded in a comment body, or "" if it carries none — a foreign comment,
 * or one posted before reconciliation existed.
 */
export function parseFpMarker(body: string): string {
  const m = FP_MARKER_PATTERN.exec(body);
  return m ? m[1]! : '';
}

/**
 * The outcome of comparing this run's inline findings against the comments already on the PR: which
 * findings to post fresh, and which existing comments to minimize.
 */
export interface ReconcileResult {
  toPost: Finding[]; // inline findings with no comment yet
  toMinimize: string[]; // node ids of comments whose finding is gone
}

/**
 * Compare this run's inline findings to the PR's existing fingerprinted review comments
 * (GitHub-as-store). A finding already represented by a comment is kept — not re-posted, so a
 * re-review is idempotent; a finding with no existing comment is posted; an existing fingerprinted
 * comment with no matching finding this run is minimized as outdated. Comments without our marker
 * (foreign, or pre-reconciliation) are ignored. `toMinimize` is sorted for deterministic behavior
 * and tests.
 */
export function reconcile(findings: Finding[], existing: ReviewCommentRef[]): ReconcileResult {
  const current = new Set(findings.map((f) => fingerprint(f)));
  const have = new Map<string, string[]>(); // fingerprint -> existing node ids
  for (const rc of existing) {
    const fp = parseFpMarker(rc.body);
    if (fp !== '') {
      const ids = have.get(fp);
      if (ids) {
        ids.push(rc.nodeId);
      } else {
        have.set(fp, [rc.nodeId]);
      }
    }
  }

  const res: ReconcileResult = { toPost: [], toMinimize: [] };
  for (const f of findings) {
    if (!have.has(fingerprint(f))) {
      res.toPost.push(f);
    }
  }
  for (const [fp, ids] of have) {
    if (!current.has(fp)) {
      res.toMinimize.push(...ids);
    }
  }
  res.toMinimize.sort();
  return res;
}
