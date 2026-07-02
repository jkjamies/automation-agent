/** The two-dimensional file-count / diff-byte size cap. */

/**
 * Report whether a filtered diff exceeds either configured cap. The gate is two-dimensional: a PR
 * is too large if it changes more than `maxFiles` files OR its filtered patches exceed
 * `maxDiffBytes` — review-or-deny, no degrade tier. A non-positive cap disables that dimension.
 * The reason is phrased for the "too large — please split" deny comment. The size is taken on the
 * *filtered* set, so excluded lockfile/vendor churn never trips the gate.
 */
export function oversize(
  fileCount: number,
  diffBytes: number,
  maxFiles: number,
  maxDiffBytes: number,
): { reason: string; denied: boolean } {
  if (maxFiles > 0 && fileCount > maxFiles) {
    return {
      reason: `${fileCount} changed files (after excluding generated files) exceeds the ${maxFiles}-file review limit`,
      denied: true,
    };
  }
  if (maxDiffBytes > 0 && diffBytes > maxDiffBytes) {
    return {
      reason: `${diffBytes} diff bytes (after excluding generated files) exceeds the ${maxDiffBytes}-byte review limit`,
      denied: true,
    };
  }
  return { reason: '', denied: false };
}
