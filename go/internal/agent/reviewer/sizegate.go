package reviewer

import "fmt"

// oversize reports whether a filtered diff exceeds either configured cap. The gate is
// two-dimensional (spec Decision 4): a PR is too large if it changes more than maxFiles
// files OR its filtered patches exceed maxDiffBytes — review-or-deny, no degrade tier. A
// non-positive cap disables that dimension. The reason is phrased for the "too large —
// please split" deny comment a later change posts. The size is taken on the *filtered* set,
// so excluded lockfile/vendor churn never trips the gate.
func oversize(fileCount, diffBytes, maxFiles, maxDiffBytes int) (reason string, denied bool) {
	if maxFiles > 0 && fileCount > maxFiles {
		return fmt.Sprintf("%d changed files (after excluding generated files) exceeds the %d-file review limit", fileCount, maxFiles), true
	}
	if maxDiffBytes > 0 && diffBytes > maxDiffBytes {
		return fmt.Sprintf("%d diff bytes (after excluding generated files) exceeds the %d-byte review limit", diffBytes, maxDiffBytes), true
	}
	return "", false
}
