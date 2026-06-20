package lintfixer

import (
	"context"
	"embed"
	"fmt"
	"iter"
	"sort"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/jkjamies/automation-agent/internal/agent/lintfixer/models"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

//go:embed prompts/*.md
var promptFS embed.FS

var prompts = setup.NewPrompts(promptFS)

const editPrefix = "edit:" // one state key per fixed file: edit:<path>

// RunAnalyze fans out one analyzer agent per affected file (the triage already
// grouped problems by file, so two agents never rewrite the same file), runs them
// in parallel, and returns the proposed whole-file edits. ciFeedback, when
// non-empty, is the previous attempt's CI failure passed to the model to correct
// its prior change.
func RunAnalyze(ctx context.Context, llm model.LLM, files []models.FileProblems, fileContents map[string]string, ciFeedback string) ([]FileEdit, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("analyze: no files to fix")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	analyzers := make([]agent.Agent, 0, len(files))
	for _, f := range files {
		analyzers = append(analyzers, newFileAnalyzer(f.Path, fileContents[f.Path], f.Problems, llm, ciFeedback))
	}

	par, err := parallelagent.New(parallelagent.Config{AgentConfig: agent.Config{
		Name:        "analyze_all",
		Description: "Fixes lint problems for each affected file in parallel",
		SubAgents:   analyzers,
	}})
	if err != nil {
		return nil, fmt.Errorf("build analyzers: %w", err)
	}

	r, err := setup.NewRunner("lint-analyze", par)
	if err != nil {
		return nil, err
	}
	state, err := setup.DriveCollectState(ctx, r, "system", "analyze", "Fix the lint problems.")
	if err != nil {
		return nil, err
	}

	edits := make([]FileEdit, 0, len(files))
	for _, f := range files {
		if v, ok := state[editPrefix+f.Path]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				edits = append(edits, FileEdit{Path: f.Path, Content: s})
			}
		}
	}
	if len(edits) == 0 {
		return nil, fmt.Errorf("analyze produced no edits")
	}
	return edits, nil
}

// newFileAnalyzer is a code agent that asks the LLM to fix one file's problems and
// writes the corrected content to state under edit:<path>.
func newFileAnalyzer(path, content string, problems []string, llm model.LLM, ciFeedback string) agent.Agent {
	name := "analyze_" + safeName(path)
	a, _ := agent.New(agent.Config{
		Name:        name,
		Description: "Fixes lint problems in " + path,
		Run: func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				out, err := setup.GenerateText(ctx, llm, prompts.MustGet("analyze"), buildFilePrompt(path, content, problems, ciFeedback))
				if err != nil {
					yield(nil, fmt.Errorf("analyze %s: %w", path, err))
					return
				}
				fixed := parseFileContent(out)
				if strings.TrimSpace(fixed) == "" {
					yield(nil, fmt.Errorf("analyze %s: model returned empty content", path))
					return
				}
				yield(setup.TextEvent(name, "fixed "+path, map[string]any{editPrefix + path: fixed}), nil)
			}
		},
	})
	return a
}

func buildFilePrompt(path, content string, problems []string, ciFeedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "File: %s\n\nLint problems to fix:\n", path)
	for _, p := range problems {
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

// parseFileContent strips surrounding markdown code fences the model may add and
// normalizes a trailing newline.
func parseFileContent(out string) string {
	s := strings.TrimSpace(out)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimRight(s, "\n") + "\n"
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
