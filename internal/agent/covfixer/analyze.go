package covfixer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jkjamies/automation-agent/internal/agent/fixflow"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

// Analyze plans test placement by examining the checked-out repo's real conventions,
// then generates a test per file in parallel from that grounded plan.
func Analyze(ctx context.Context, in fixflow.AnalyzeInput) ([]fixflow.FileEdit, error) {
	plan, err := explore(ctx, in)
	if err != nil {
		return nil, err
	}
	return execute(ctx, in, plan)
}

// planEntry is the explorer's decision for one source file, grounded in the repo's
// actual existing tests (never derived from a fixed rule).
type planEntry struct {
	Source    string `json:"source"`
	TestPath  string `json:"test_path"`
	Framework string `json:"framework"`
	Notes     string `json:"notes"`
}

// explore gathers the repo's real test conventions from the checkout and asks the
// model to produce a per-file plan grounded in them.
func explore(ctx context.Context, in fixflow.AnalyzeInput) (map[string]planEntry, error) {
	conventions := gatherTestConventions(in.RepoDir)
	out, err := setup.GenerateText(ctx, in.LLM, prompts.MustGet("explore"), buildExploreInput(in.Work, conventions))
	if err != nil {
		return nil, fmt.Errorf("explore: %w", err)
	}
	plan, err := parsePlan(out)
	if err != nil {
		return nil, fmt.Errorf("explore: %w", err)
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("explore: produced no test placements")
	}
	return plan, nil
}

// execute writes each test from the plan + source, one parallel agent per file.
func execute(ctx context.Context, in fixflow.AnalyzeInput, plan map[string]planEntry) ([]fixflow.FileEdit, error) {
	return fixflow.ParallelAnalyze(ctx, in.Work, func(ctx context.Context, w fixflow.FileWork) (fixflow.FileEdit, error) {
		p, ok := plan[w.Path]
		if !ok || strings.TrimSpace(p.TestPath) == "" {
			return fixflow.FileEdit{}, nil // explorer couldn't place it -> skip
		}
		src, err := fixflow.ReadFile(in.RepoDir, w.Path)
		if err != nil {
			return fixflow.FileEdit{}, nil
		}
		out, err := setup.GenerateText(ctx, in.Coder(), prompts.MustGet("analyze"), buildExecuteInput(w, src, p, in.Feedback))
		if err != nil {
			return fixflow.FileEdit{}, err
		}
		return fixflow.FileEdit{Path: p.TestPath, Content: fixflow.StripFences(out)}, nil
	})
}

func parsePlan(out string) (map[string]planEntry, error) {
	js := fixflow.ExtractJSONArray(out)
	if js == "" {
		return nil, fmt.Errorf("no JSON array in explorer output")
	}
	var entries []planEntry
	if err := json.Unmarshal([]byte(js), &entries); err != nil {
		return nil, fmt.Errorf("decode plan JSON: %w", err)
	}
	m := make(map[string]planEntry, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.Source) != "" {
			m[e.Source] = e
		}
	}
	return m, nil
}

func buildExploreInput(work []fixflow.FileWork, conventions string) string {
	var b strings.Builder
	b.WriteString("Source files that need tests:\n")
	for _, w := range work {
		fmt.Fprintf(&b, "- %s\n", w.Path)
	}
	b.WriteString("\n")
	b.WriteString(conventions)
	return b.String()
}

func buildExecuteInput(w fixflow.FileWork, src string, p planEntry, ciFeedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write the test file at: %s\nFramework / convention: %s\n", p.TestPath, p.Framework)
	if strings.TrimSpace(p.Notes) != "" {
		fmt.Fprintf(&b, "Notes: %s\n", p.Notes)
	}
	b.WriteString("\nUncovered logic to cover:\n")
	for _, u := range w.Items {
		fmt.Fprintf(&b, "- %s\n", u)
	}
	if ciFeedback != "" {
		b.WriteString("\nThe previous attempt failed CI with:\n")
		b.WriteString(ciFeedback)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nSource file (%s):\n```\n%s\n```\n", w.Path, src)
	return b.String()
}

// gatherTestConventions walks the checkout for existing test files (real evidence of
// the project's conventions) and includes one example, so the explorer decides test
// placement from what the repo actually does — not a hardcoded rule.
func gatherTestConventions(repoDir string) string {
	tests := findTestFiles(repoDir)
	var b strings.Builder
	if len(tests) == 0 {
		b.WriteString("No existing test files were found in the repository. Infer the idiomatic location and framework for the language, and state your assumption in 'notes'.\n")
		return b.String()
	}
	b.WriteString("Existing test files in this repository (use these to determine the real conventions — location, naming, framework):\n")
	for _, t := range tests {
		fmt.Fprintf(&b, "- %s\n", t)
	}
	if ex, err := fixflow.ReadFile(repoDir, tests[0]); err == nil {
		fmt.Fprintf(&b, "\nExample existing test file (%s):\n```\n%s\n```\n", tests[0], truncate(ex, 4000))
	}
	return b.String()
}

func findTestFiles(repoDir string) []string {
	var out []string
	_ = filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if isTestFile(d.Name()) {
			if rel, err := filepath.Rel(repoDir, p); err == nil {
				out = append(out, rel)
			}
		}
		if len(out) >= 60 {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func isTestFile(name string) bool {
	switch {
	case strings.HasSuffix(name, "_test.go"),
		strings.HasSuffix(name, ".test.ts"), strings.HasSuffix(name, ".test.tsx"),
		strings.HasSuffix(name, ".test.js"), strings.HasSuffix(name, ".spec.ts"),
		strings.HasSuffix(name, ".spec.js"),
		strings.HasSuffix(name, "_test.py"), strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py"),
		strings.HasSuffix(name, "_spec.rb"),
		strings.HasSuffix(name, "Test.java"), strings.HasSuffix(name, "Tests.java"),
		strings.HasSuffix(name, "Test.kt"), strings.HasSuffix(name, "Tests.kt"),
		strings.HasSuffix(name, "Tests.swift"),
		strings.HasSuffix(name, "_test.rs"):
		return true
	}
	return false
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "target", "build", ".gradle", "dist", "out", ".idea":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated)"
}
