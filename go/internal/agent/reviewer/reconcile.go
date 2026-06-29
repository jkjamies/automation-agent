package reviewer

import (
	"regexp"
	"sort"

	"automation-agent/internal/githubapi"
)

// The hidden fingerprint marker tags each inline comment with the fingerprint of the finding that
// produced it, so a later re-review re-identifies the comment from GitHub itself (GitHub-as-store —
// no local durable state). It is an HTML comment appended to the body and is an external-ish
// contract: keep the exact format stable across ports.
const (
	fpMarkerPrefix = "<!-- ar-fp:"
	fpMarkerSuffix = " -->"
)

// fpMarkerPattern extracts the fingerprint from a comment body. Non-greedy so a body with trailing
// content still matches only the marker payload.
var fpMarkerPattern = regexp.MustCompile(`<!-- ar-fp:(.+?) -->`)

// fpMarker renders the hidden fingerprint marker appended to an inline comment body.
func fpMarker(fingerprint string) string {
	return fpMarkerPrefix + fingerprint + fpMarkerSuffix
}

// parseFPMarker returns the fingerprint embedded in a comment body, or "" if it carries none — a
// foreign comment, or one posted before reconciliation existed.
func parseFPMarker(body string) string {
	m := fpMarkerPattern.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// reconcileResult is the outcome of comparing this run's inline findings against the comments
// already on the PR: which findings to post fresh, and which existing comments to minimize.
type reconcileResult struct {
	toPost     []Finding // inline findings with no existing comment yet
	toMinimize []string  // node ids of fingerprinted comments whose finding is gone this run
}

// reconcile compares this run's inline findings to the PR's existing fingerprinted review comments
// (GitHub-as-store). A finding already represented by a comment is kept — not re-posted, so a
// re-review is idempotent; a finding with no existing comment is posted; an existing fingerprinted
// comment with no matching finding this run is minimized as outdated (the finding was fixed or no
// longer applies). Comments without our marker (foreign, or pre-reconciliation) are ignored.
// toMinimize is sorted for deterministic behavior and tests.
func reconcile(findings []Finding, existing []githubapi.ReviewCommentRef) reconcileResult {
	current := make(map[string]bool, len(findings))
	for _, f := range findings {
		current[f.fingerprint()] = true
	}
	have := map[string][]string{} // fingerprint -> existing node ids
	for _, rc := range existing {
		if fp := parseFPMarker(rc.Body); fp != "" {
			have[fp] = append(have[fp], rc.NodeID)
		}
	}

	var res reconcileResult
	for _, f := range findings {
		if _, ok := have[f.fingerprint()]; !ok {
			res.toPost = append(res.toPost, f)
		}
	}
	for fp, ids := range have {
		if !current[fp] {
			res.toMinimize = append(res.toMinimize, ids...)
		}
	}
	sort.Strings(res.toMinimize)
	return res
}
