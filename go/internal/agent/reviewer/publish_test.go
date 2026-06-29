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
	findings := []Finding{
		{File: "a.go", Line: 2, Dimension: DimSecurity, Severity: SeverityCritical, Message: "sqli", Suggestion: "safe()", FixPrompt: "fix it"},
		{File: "a.go", Line: 99, Dimension: DimPerformance, Severity: SeverityMajor, Message: "n+1 query"}, // out of diff
		{File: "b.go", Line: 1, Dimension: DimMaintainability, Severity: SeverityNitpick, Message: "rename"},
	}
	card := scoreFindings(findings)
	gh := &fakeGH{}
	meta := publishMeta{owner: "o", repo: "r", number: 7, headSHA: "sha1", files: files}
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
	for _, want := range []string{"🔒 Security", "```suggestion", "safe()", "Prompt for AI agents", "fix it"} {
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
	for _, want := range []string{"automation-agent:review:o/r#7", "Agent review", "Outside diff range (1)", "Nitpicks (1)", "n+1 query"} {
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

// A redelivered task for a head SHA already published posts nothing (at-least-once safety).
func TestPublishIdempotentOnRepublishedSHA(t *testing.T) {
	gh := &fakeGH{agentCheck: githubapi.CheckResult{Found: true}}
	files := []githubapi.PRFile{{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n+x\n"}}
	findings := []Finding{{File: "a.go", Line: 1, Dimension: DimSecurity, Severity: SeverityCritical, Message: "x"}}
	meta := publishMeta{owner: "o", repo: "r", number: 1, headSHA: "s", files: files}
	if err := testEngine(gh).publish(context.Background(), scoreFindings(findings), findings, meta); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if gh.review != nil || len(gh.upserts) != 0 || len(gh.checks) != 0 {
		t.Errorf("a republished SHA must post nothing: review=%v upserts=%d checks=%d", gh.review, len(gh.upserts), len(gh.checks))
	}
}

// sanitizeText defuses @mentions and escapes HTML so model-authored findings can't ping users or
// inject markup; the ```suggestion code path is left untouched by callers.
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
