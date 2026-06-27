package fixflow

import (
	"fmt"
	"strings"
	"testing"

	"automation-agent/internal/githubapi"
)

func cmp(commits int, files ...string) githubapi.Comparison {
	c := githubapi.Comparison{TotalCommits: commits}
	for _, f := range files {
		c.Files = append(c.Files, githubapi.ChangedFile{Path: f, Status: "modified"})
	}
	return c
}

func TestBuildSummarySuccess(t *testing.T) {
	text := buildSummaryText(summaryInput{
		outcome: outcomeSuccess, workflow: "lint", fullRepo: "acme/api", prNumber: 7,
		attempts: 2, report: "fix unused var in a.go", changed: cmp(2, "a.go", "b.go"),
	})
	for _, want := range []string{"acme/api", "passed CI", "2 attempts", "2 commits", "a.go, b.go", "Targeted:"} {
		if !strings.Contains(text, want) {
			t.Errorf("success summary missing %q in:\n%s", want, text)
		}
	}
}

func TestBuildSummaryExhausted(t *testing.T) {
	text := buildSummaryText(summaryInput{
		outcome: outcomeExhausted, workflow: "coverage", fullRepo: "acme/api", attempts: 3,
		lastOutput: "still 40% covered", changed: cmp(1, "x.go"),
	})
	for _, want := range []string{"still fails CI", "3 attempts", "1 commit", "Remaining: still 40% covered"} {
		if !strings.Contains(text, want) {
			t.Errorf("exhausted summary missing %q in:\n%s", want, text)
		}
	}
}

func TestBuildSummaryTimeout(t *testing.T) {
	text := buildSummaryText(summaryInput{
		outcome: outcomeTimeout, workflow: "lint", fullRepo: "acme/api", attempts: 1,
		timeout: "90m0s", checkName: "agent-lint-verify", report: "x", changed: cmp(0),
	})
	for _, want := range []string{"no CI result after 90m0s", "agent-lint-verify", "1 attempt", "No changes were recorded"} {
		if !strings.Contains(text, want) {
			t.Errorf("timeout summary missing %q in:\n%s", want, text)
		}
	}
}

// The clean outcome is a workflow-prefixed fun line plus the repo, deterministically chosen
// by repo name — never the review alarm. The exact string is locked for cross-port parity.
func TestBuildSummaryClean(t *testing.T) {
	text := buildSummaryText(summaryInput{outcome: outcomeClean, workflow: "lint", fullRepo: "acme/api"})
	want := "Lint: all tidy already — I'll see myself out 🚪 — acme/api is already clean, no PR opened."
	if text != want {
		t.Errorf("clean summary =\n%q\nwant\n%q", text, want)
	}
	if strings.Contains(text, "review") {
		t.Errorf("clean summary must not mention review: %q", text)
	}
	// A different repo rotates to a different line (deterministic, but varied).
	other := buildSummaryText(summaryInput{outcome: outcomeClean, workflow: "coverage", fullRepo: "acme/web"})
	if !strings.HasPrefix(other, "Coverage: ") {
		t.Errorf("clean summary should be prefixed by the workflow: %q", other)
	}
}

// Long findings are truncated with an ellipsis; a long file list collapses to the first
// few plus a "+N more" tail.
func TestBuildSummaryTruncation(t *testing.T) {
	files := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		files = append(files, fmt.Sprintf("f%d.go", i))
	}
	text := buildSummaryText(summaryInput{
		outcome: outcomeSuccess, workflow: "lint", fullRepo: "r", attempts: 1,
		report: strings.Repeat("x", 500), changed: cmp(20, files...),
	})
	if !strings.Contains(text, "…") {
		t.Error("expected findings to be truncated with an ellipsis")
	}
	if !strings.Contains(text, "(+12 more)") {
		t.Errorf("expected the file list truncated to 8 with a +12 more tail, got:\n%s", text)
	}
}
