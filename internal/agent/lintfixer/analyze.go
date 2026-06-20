package lintfixer

import (
	"context"
	"fmt"
	"strings"

	"github.com/jkjamies/automation-agent/internal/agent/fixflow"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

// Analyze rewrites each affected source file to fix its lint problems, one parallel
// agent per file, reading the current source from the checkout. Feedback (from a
// retry) is the previous attempt's CI failure.
func Analyze(ctx context.Context, in fixflow.AnalyzeInput) ([]fixflow.FileEdit, error) {
	return fixflow.ParallelAnalyze(ctx, in.Work, func(ctx context.Context, w fixflow.FileWork) (fixflow.FileEdit, error) {
		src, err := fixflow.ReadFile(in.RepoDir, w.Path)
		if err != nil {
			return fixflow.FileEdit{}, nil // unreadable file -> skip
		}
		out, err := setup.GenerateText(ctx, in.Coder(), prompts.MustGet("analyze"), buildFilePrompt(w, src, in.Feedback))
		if err != nil {
			return fixflow.FileEdit{}, err
		}
		return fixflow.FileEdit{Path: w.Path, Content: fixflow.StripFences(out)}, nil
	})
}

func buildFilePrompt(w fixflow.FileWork, content, ciFeedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "File: %s\n\nLint problems to fix:\n", w.Path)
	for _, p := range w.Items {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	if ciFeedback != "" {
		b.WriteString("\nThe previous attempt failed CI with:\n")
		b.WriteString(ciFeedback)
		b.WriteString("\n")
	}
	b.WriteString("\nCurrent file content:\n```\n")
	b.WriteString(content)
	b.WriteString("\n```\n\nOutput ONLY the complete corrected content of this file — no explanation, no markdown fences.")
	return b.String()
}
