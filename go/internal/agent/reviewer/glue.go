package reviewer

// This file holds the deterministic verify-gate and merge logic that the glue/synthesis pass
// owns (spec Decisions 3, 5, 13). The glue *agent* itself (architectural alignment,
// testability, and test-coverage reasoning) is wired in agents_setup.go and run in review.go;
// cross-lens dedup and the confidence gate are done here in code rather than asked of the
// model, so they are deterministic and unit-testable.

// dropLowConfidence removes findings below the configured minimum confidence (spec Decision
// 13's phase-1 verify gate). A non-positive minimum keeps everything.
func dropLowConfidence(findings []Finding, min float64) []Finding {
	if min <= 0 {
		return findings
	}
	out := findings[:0:0] // new backing array; never alias the caller's slice
	for _, f := range findings {
		if f.Confidence >= min {
			out = append(out, f)
		}
	}
	return out
}

// dedupe collapses findings that share a fingerprint (same file+line+message, across lenses),
// keeping the one with the worst severity (ties broken by higher confidence). The glue pass's
// job of removing the same line flagged by multiple lenses (spec Decision 7/3) is done here
// deterministically. Input order is otherwise preserved.
func dedupe(findings []Finding) []Finding {
	seen := make(map[string]int, len(findings)) // fingerprint -> index in out
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		fp := f.fingerprint()
		if i, ok := seen[fp]; ok {
			if better(f, out[i]) {
				out[i] = f
			}
			continue
		}
		seen[fp] = len(out)
		out = append(out, f)
	}
	return out
}

// better reports whether a should replace b among duplicates: worse severity wins; on a tie,
// higher confidence.
func better(a, b Finding) bool {
	if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
		return ra > rb
	}
	return a.Confidence > b.Confidence
}

// demoteToNitpick forces every finding to nitpick severity. The catch-all "(other)" category
// is intentionally low-signal, so its findings are demoted rather than allowed to drive the
// scorecard (spec Decision 3).
func demoteToNitpick(findings []Finding) []Finding {
	for i := range findings {
		findings[i].Severity = SeverityNitpick
	}
	return findings
}
