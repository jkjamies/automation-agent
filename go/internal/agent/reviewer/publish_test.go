package reviewer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"automation-agent/internal/githubapi"
)

func TestPublishRoutesFindings(t *testing.T) {
	// a.go's hunk makes head lines 1 (context), 2 and 3 (added) commentable.
	files := []githubapi.PRFile{{Path: "a.go", Status: "modified", Patch: "@@ -1,2 +1,3 @@\n a\n+b\n+c\n"}}
	findings := []finding{
		{File: "a.go", Line: 2, Dimension: DimSecurity, Severity: SeverityCritical, Message: "sqli", Suggestion: "Bind the id as a query parameter.", FixPrompt: "fix it"},
		{File: "a.go", Line: 99, Dimension: DimPerformance, Severity: SeverityMajor, Message: "n+1 query"}, // out of diff
		{File: "b.go", Line: 1, Dimension: DimMaintainability, Severity: SeverityNitpick, Message: "rename"},
	}
	card := scoreFindings(findings)
	gh := &fakeGH{}
	meta := publishMeta{owner: "o", repo: "r", number: 7, headSHA: "sha1", files: files,
		lenses: []lensStat{{lens: categories[1], model: "m", ran: true, usage: true, tokensIn: 10, tokensOut: 2}}}
	if err := testEngine(gh).publish(context.Background(), card, findings, meta); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// One inline comment (the in-diff security finding) with prefix, suggestion, and AI prompt.
	if gh.review == nil || len(gh.review.Comments) != 1 {
		t.Fatalf("want 1 inline comment, got %+v", gh.review)
	}
	c := gh.review.Comments[0]
	if c.Path != "a.go" || c.Line != 2 || c.Side != "RIGHT" {
		t.Errorf("inline target = %+v, want a.go:2 RIGHT", c)
	}
	// The review must be pinned to the SHA the diff (and so every line number above) came from.
	// Unpinned, GitHub resolves the lines against whatever HEAD is current when the call lands,
	// which after a mid-review push means wrong anchors or a 422 that takes the whole publish down.
	if gh.review.CommitID != meta.headSHA {
		t.Errorf("review CommitID = %q, want the reviewed head SHA %q", gh.review.CommitID, meta.headSHA)
	}
	for _, want := range []string{"🔒 Security", "**Suggestion:** Bind the id as a query parameter.", "Prompt for AI agents"} {
		if !strings.Contains(c.Body, want) {
			t.Errorf("inline body missing %q:\n%s", want, c.Body)
		}
	}

	// Summary upserted once: marker, scorecard, out-of-diff section, nitpicks section.
	if len(gh.upserts) != 1 {
		t.Fatalf("want 1 summary upsert, got %d", len(gh.upserts))
	}
	sum := gh.upserts[0]
	if !strings.Contains(sum.body, sum.marker) {
		t.Error("summary body must embed its marker")
	}
	for _, want := range []string{"automation-agent:review:o/r#7", "Agent review", "Outside diff range (1)", "Nitpicks (1)", "a.go:99",
		"| Lens | Level | Model | Time | Tokens in | Tokens out |", "| Security | 🔴 | `m` |"} {
		if !strings.Contains(sum.body, want) {
			t.Errorf("summary missing %q:\n%s", want, sum.body)
		}
	}

	// One check; a security-critical caps overall to red → neutral (advisory, never failure).
	if len(gh.checks) != 1 || gh.checks[0].Name != "agent-review" || gh.checks[0].Conclusion != "neutral" || gh.checks[0].HeadSHA != "sha1" {
		t.Errorf("check = %+v, want agent-review/neutral/sha1", gh.checks)
	}
}

func TestPublishCleanPRPostsSuccess(t *testing.T) {
	gh := &fakeGH{}
	meta := publishMeta{owner: "o", repo: "r", number: 1, headSHA: "s", files: []githubapi.PRFile{{Path: "a.go"}}}
	if err := testEngine(gh).publish(context.Background(), scoreFindings(nil), nil, meta); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if gh.review != nil {
		t.Error("a clean PR posts no inline review")
	}
	if len(gh.checks) != 1 || gh.checks[0].Conclusion != "success" {
		t.Errorf("clean check = %+v, want success", gh.checks)
	}
	if len(gh.upserts) != 1 || !strings.Contains(gh.upserts[0].body, "No findings") {
		t.Errorf("clean summary = %+v, want a 'No findings' note", gh.upserts)
	}
}

func TestPublishDeny(t *testing.T) {
	gh := &fakeGH{}
	meta := publishMeta{owner: "o", repo: "r", number: 3, headSHA: "s"}
	if err := testEngine(gh).publishDeny(context.Background(), meta, "too big", 200, 999999); err != nil {
		t.Fatalf("publishDeny: %v", err)
	}
	if gh.review != nil {
		t.Error("deny posts no inline review")
	}
	if len(gh.checks) != 1 || gh.checks[0].Conclusion != "neutral" {
		t.Errorf("deny check = %+v, want neutral", gh.checks)
	}
	if len(gh.upserts) != 1 || !strings.Contains(gh.upserts[0].body, "too large") {
		t.Errorf("deny summary = %+v, want 'too large'", gh.upserts)
	}
}

func TestPublishWriteErrorPropagates(t *testing.T) {
	gh := &fakeGH{writeErr: errors.New("boom")}
	meta := publishMeta{owner: "o", repo: "r", number: 1, headSHA: "s"}
	if err := testEngine(gh).publish(context.Background(), scoreFindings(nil), nil, meta); err == nil {
		t.Fatal("a write failure must propagate so the dispatch retries")
	}
}

// Re-review reconciles against GitHub: a finding already on the PR (its fingerprint marker is on an
// existing comment) is not re-posted (idempotent), and an existing comment whose finding is gone is
// minimized. Comments without our marker are left alone.
// A redelivered task for a head SHA already published posts nothing: reconciliation makes the
// comments idempotent, and the guard keeps the check run and summary from duplicating.
func TestPublishIdempotentOnRepublishedSHA(t *testing.T) {
	gh := &fakeGH{agentCheck: githubapi.CheckResult{Found: true}}
	files := []githubapi.PRFile{{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n+x\n"}}
	findings := []finding{{File: "a.go", Line: 1, Dimension: DimSecurity, Severity: SeverityCritical, Message: "x"}}
	meta := publishMeta{owner: "o", repo: "r", number: 1, headSHA: "s", files: files}
	if err := testEngine(gh).publish(context.Background(), scoreFindings(findings), findings, meta); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if gh.review != nil || len(gh.upserts) != 0 || len(gh.checks) != 0 || len(gh.minimized) != 0 {
		t.Errorf("a republished SHA must post nothing: review=%v upserts=%d checks=%d minimized=%d",
			gh.review, len(gh.upserts), len(gh.checks), len(gh.minimized))
	}
}

func TestPublishReconciles(t *testing.T) {
	files := []githubapi.PRFile{{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n+x\n"}}
	sqli := finding{File: "a.go", Line: 1, Dimension: DimSecurity, Severity: SeverityCritical, Message: "sqli"}
	gh := &fakeGH{existing: []githubapi.ReviewCommentRef{
		{NodeID: "keep", Body: "old body " + fpMarker(sqli.fingerprint())},
		{NodeID: "stale", Body: "fixed finding " + fpMarker("a.go:9:obsolete")},
		{NodeID: "foreign", Body: "a human comment with no marker"},
	}}
	meta := publishMeta{owner: "o", repo: "r", number: 1, headSHA: "s", files: files}
	if err := testEngine(gh).publish(context.Background(), scoreFindings([]finding{sqli}), []finding{sqli}, meta); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if gh.review != nil {
		t.Errorf("a finding already on the PR must not be re-posted: %+v", gh.review)
	}
	if len(gh.minimized) != 1 || gh.minimized[0] != "stale" {
		t.Errorf("minimized = %v, want [stale]", gh.minimized)
	}
}

// Minimizing an outdated comment is best-effort: a failure there must not abort publish, so the
// summary comment and check run still post (otherwise a retry short-circuits at alreadyPublished
// and the PR is left without its summary/check).
func TestPublishMinimizeFailureStillPublishes(t *testing.T) {
	files := []githubapi.PRFile{{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n+x\n"}}
	fnd := finding{File: "a.go", Line: 1, Dimension: DimSecurity, Severity: SeverityCritical, Message: "new"}
	gh := &fakeGH{
		minimizeErr: errors.New("graphql boom"),
		existing:    []githubapi.ReviewCommentRef{{NodeID: "stale", Body: "fixed " + fpMarker("a.go:9:obsolete")}},
	}
	meta := publishMeta{owner: "o", repo: "r", number: 1, headSHA: "s", files: files}
	if err := testEngine(gh).publish(context.Background(), scoreFindings([]finding{fnd}), []finding{fnd}, meta); err != nil {
		t.Fatalf("publish must not fail when minimize fails: %v", err)
	}
	if len(gh.minimized) != 1 || gh.minimized[0] != "stale" {
		t.Errorf("minimize must be attempted, got %v", gh.minimized)
	}
	if len(gh.upserts) != 1 {
		t.Errorf("summary comment must still be upserted despite minimize failure, got %d", len(gh.upserts))
	}
	if len(gh.checks) != 1 {
		t.Errorf("check run must still be created despite minimize failure, got %d", len(gh.checks))
	}
}

// A finding with no existing comment is posted, carrying its fingerprint marker for next time.
func TestPublishPostsNewFinding(t *testing.T) {
	files := []githubapi.PRFile{{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n+x\n"}}
	fnd := finding{File: "a.go", Line: 1, Dimension: DimSecurity, Severity: SeverityCritical, Message: "new"}
	gh := &fakeGH{} // no existing comments
	meta := publishMeta{owner: "o", repo: "r", number: 1, headSHA: "s", files: files}
	if err := testEngine(gh).publish(context.Background(), scoreFindings([]finding{fnd}), []finding{fnd}, meta); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if gh.review == nil || len(gh.review.Comments) != 1 {
		t.Fatalf("a new finding must be posted, got %+v", gh.review)
	}
	if !strings.Contains(gh.review.Comments[0].Body, fpMarker(fnd.fingerprint())) {
		t.Error("posted comment must carry its fingerprint marker")
	}
	if len(gh.minimized) != 0 {
		t.Errorf("nothing to minimize, got %v", gh.minimized)
	}
}

// A suggestion is rendered as sanitized prose, never as a GitHub ```suggestion block: the lenses
// see only a diff, so a verbatim replacement would misalign with the lines it claims to replace,
// and the one-click "apply" would commit it. As prose it also goes through the same @mention /
// HTML defusing as the message.
func TestInlineCommentSuggestionIsProse(t *testing.T) {
	f := finding{File: "a.go", Line: 2, Dimension: DimSecurity, Severity: SeverityCritical, Message: "sqli",
		Suggestion: "Ask @octocat to use <code>Bind</code>; see ```go``` docs."}
	body := inlineCommentBody(f)
	if strings.Contains(body, "```suggestion") || strings.Contains(body, "````") {
		t.Errorf("suggestion must not render as a suggestion/code block:\n%s", body)
	}
	if !strings.Contains(body, "**Suggestion:** Ask @\u200boctocat to use &lt;code&gt;Bind&lt;/code&gt;") {
		t.Errorf("suggestion prose not rendered sanitized:\n%s", body)
	}
	if strings.Contains(inlineCommentBody(finding{Message: "m", Suggestion: "  \n"}), "Suggestion") {
		t.Error("a blank suggestion must render nothing")
	}
}

// sanitizeText defuses @mentions and escapes HTML so model-authored findings can't ping users or
// inject markup; the fenced FixPrompt path is left untouched by callers.
func TestSanitizeText(t *testing.T) {
	got := sanitizeText("ping @octocat with <b>x</b> & </details>")
	if strings.Contains(got, "@octocat") {
		t.Errorf("mention not defused: %q", got)
	}
	if strings.Contains(got, "<b>") || strings.Contains(got, "</details>") {
		t.Errorf("HTML not escaped: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;") || !strings.Contains(got, "&amp;") {
		t.Errorf("expected escaped entities: %q", got)
	}
}

func TestCheckConclusionAdvisory(t *testing.T) {
	if checkConclusion(levelGreen) != "success" {
		t.Error("green → success")
	}
	for _, l := range []level{levelYellow, levelRed} {
		if got := checkConclusion(l); got != "neutral" {
			t.Errorf("checkConclusion(%v) = %q, want neutral (never failure)", l, got)
		}
	}
}
