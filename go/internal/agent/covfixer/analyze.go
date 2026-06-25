package covfixer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"automation-agent/internal/agent/fixflow"
	"automation-agent/internal/agent/setup"
)

// Analyze plans test placement by having a tool-using agent examine the checked-out
// repo's real conventions, then generates a test per file in parallel from that plan.
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

// explore runs a tool-using agent that navigates the checkout itself (read_file /
// list_dir) to learn the repo's real test conventions and returns a per-file plan.
func explore(ctx context.Context, in fixflow.AnalyzeInput) (map[string]planEntry, error) {
	out, err := fixflow.Explore(ctx, in.LLM, in.RepoDir, prompts.MustGet("explore"), buildExploreInput(in.Work))
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
			// Explorer couldn't place a test for this file -> skip. Log it so a skip is
			// distinguishable from "nothing to do".
			in.Logger().Warn("coverage analyze: skipping file with no test placement", "path", w.Path)
			return fixflow.FileEdit{}, nil
		}
		src, err := fixflow.ReadFile(in.RepoDir, w.Path)
		if err != nil {
			in.Logger().Warn("coverage analyze: skipping unreadable file", "path", w.Path, "err", err)
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

func buildExploreInput(work []fixflow.FileWork) string {
	var b strings.Builder
	b.WriteString("Source files that need tests:\n")
	for _, w := range work {
		fmt.Fprintf(&b, "- %s\n", w.Path)
	}
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
