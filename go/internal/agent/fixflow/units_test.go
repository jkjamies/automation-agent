package fixflow

import (
	"context"
	"strings"
	"testing"
)

func TestParseKickoff(t *testing.T) {
	k, err := ParseKickoff([]byte(`{"repo":"acme/api","report":{"x":1}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if k.Owner() != "acme" || k.Name() != "api" || k.Base != "main" || k.ReportText() == "" {
		t.Errorf("kickoff = %+v", k)
	}
	for name, body := range map[string]string{
		"bad json":       `{`,
		"missing repo":   `{"report":{"x":1}}`,
		"bad repo":       `{"repo":"noslash","report":{"x":1}}`,
		"missing report": `{"repo":"a/b"}`,
	} {
		if _, err := ParseKickoff([]byte(body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestReportText(t *testing.T) {
	// A JSON-value report (a linter that emits JSON) is passed through as-is.
	k, err := ParseKickoff([]byte(`{"repo":"a/b","report":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if k.ReportText() != `{"x":1}` {
		t.Errorf("json value report = %q", k.ReportText())
	}
	// A JSON-string report (wrapping text/XML like lcov or JaCoCo) is unquoted.
	k2, err := ParseKickoff([]byte(`{"repo":"a/b","report":"TN:\nSF:calc.go\nDA:7,0\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if k2.ReportText() != "TN:\nSF:calc.go\nDA:7,0\n" {
		t.Errorf("string report = %q", k2.ReportText())
	}
}

func TestExtractAndStrip(t *testing.T) {
	if ExtractJSONArray("noise [1,2] x") != "[1,2]" {
		t.Error("array")
	}
	if ExtractJSONArray("none") != "" {
		t.Error("array empty")
	}
	if ExtractJSONObject("x {\"a\":1} y") != `{"a":1}` {
		t.Error("object")
	}
	if ExtractJSONObject("none") != "" {
		t.Error("object empty")
	}
	// Trailing prose with a stray bracket: the first complete value is returned (the old
	// first-bracket-to-last-bracket heuristic over-grabbed and failed to parse).
	if ExtractJSONArray(`[{"a":1}] then see [2]`) != `[{"a":1}]` {
		t.Error("array trailing prose")
	}
	if ExtractJSONObject(`{"a":1} note: closing }`) != `{"a":1}` {
		t.Error("object trailing prose")
	}
	if StripFences("```go\npackage x\n```") != "package x\n" {
		t.Error("fenced")
	}
	if StripFences("package y") != "package y\n" {
		t.Error("plain")
	}
}

func TestParallelAnalyze(t *testing.T) {
	work := []FileWork{{Path: "b.go"}, {Path: "a.go"}}
	edits, err := ParallelAnalyze(context.Background(), work, func(_ context.Context, w FileWork) (FileEdit, error) {
		return FileEdit{Path: w.Path + "_test.go", Content: "package x\n"}, nil
	})
	if err != nil {
		t.Fatalf("ParallelAnalyze: %v", err)
	}
	if len(edits) != 2 {
		t.Fatalf("edits = %d, want 2", len(edits))
	}
	// collected/sorted by original work path
	if edits[0].Path != "a.go_test.go" || edits[1].Path != "b.go_test.go" {
		t.Errorf("paths = %v", edits)
	}
}

func TestParallelAnalyzeSkips(t *testing.T) {
	_, err := ParallelAnalyze(context.Background(), []FileWork{{Path: "a.go"}}, func(_ context.Context, _ FileWork) (FileEdit, error) {
		return FileEdit{}, nil // skip
	})
	if err == nil || !strings.Contains(err.Error(), "no edits") {
		t.Fatalf("expected 'no edits' error, got %v", err)
	}
	if _, err := ParallelAnalyze(context.Background(), nil, nil); err == nil {
		t.Fatal("expected error with no work")
	}
}

func TestUniqueAnalyzerName(t *testing.T) {
	seen := map[string]int{}
	got := []string{
		uniqueAnalyzerName(seen, "a/b.kt"),
		uniqueAnalyzerName(seen, "a-b.kt"),
		uniqueAnalyzerName(seen, "a.b.kt"),
		uniqueAnalyzerName(seen, "x/y.go"),
	}
	want := []string{"analyze_a_b_kt", "analyze_a_b_kt_2", "analyze_a_b_kt_3", "analyze_x_y_go"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRepoAllowed(t *testing.T) {
	// Empty allowlist (REPOS unset) imposes no restriction.
	if open := (&Engine{d: Deps{}}); !open.repoAllowed("anyone/anything") {
		t.Error("empty allowlist should allow any repo")
	}
	// A configured allowlist admits only listed repos.
	e := &Engine{d: Deps{Repos: []string{"acme/api", "acme/web"}}}
	if !e.repoAllowed("acme/api") {
		t.Error("listed repo should be allowed")
	}
	if e.repoAllowed("evil/x") {
		t.Error("unlisted repo should be rejected")
	}
}
