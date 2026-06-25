package fixflow

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/session"

	"automation-agent/internal/agent/setup"
)

const (
	editPrefix = "edit:" // state key per file: edit:<workPath> -> new content
	pathPrefix = "path:" // state key per file: path:<workPath> -> target edit path
)

// EditFunc produces the edit for one file's work: the target path (which may differ
// from the source path — e.g. a test file) and the new content. Return a zero
// FileEdit (empty Path or Content) to skip this file.
type EditFunc func(ctx context.Context, w FileWork) (FileEdit, error)

// ParallelAnalyze fans out one analyzer agent per FileWork (ADK parallel agents,
// each writing distinct state keys so they never collide), calls fn for each, and
// returns the collected non-empty edits sorted by path.
func ParallelAnalyze(ctx context.Context, work []FileWork, fn EditFunc) ([]FileEdit, error) {
	if len(work) == 0 {
		return nil, fmt.Errorf("analyze: no files to work on")
	}
	sorted := append([]FileWork(nil), work...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	agents := make([]agent.Agent, 0, len(sorted))
	seen := make(map[string]int, len(sorted))
	for _, w := range sorted {
		a, err := newAnalyzer(uniqueAnalyzerName(seen, w.Path), w, fn)
		if err != nil {
			return nil, fmt.Errorf("build analyzer for %s: %w", w.Path, err)
		}
		agents = append(agents, a)
	}
	par, err := parallelagent.New(parallelagent.Config{AgentConfig: agent.Config{
		Name: "analyze_all", Description: "Per-file analysis in parallel", SubAgents: agents,
	}})
	if err != nil {
		return nil, fmt.Errorf("build analyzers: %w", err)
	}
	r, err := setup.NewRunner("fix-analyze", par)
	if err != nil {
		return nil, err
	}
	state, err := setup.DriveCollectState(ctx, r, "system", "analyze", "Produce the edits.")
	if err != nil {
		return nil, err
	}

	edits := make([]FileEdit, 0, len(sorted))
	for _, w := range sorted {
		content, _ := state[editPrefix+w.Path].(string)
		path, _ := state[pathPrefix+w.Path].(string)
		if strings.TrimSpace(content) != "" && path != "" {
			edits = append(edits, FileEdit{Path: path, Content: content})
		}
	}
	if len(edits) == 0 {
		return nil, fmt.Errorf("analyze produced no edits")
	}
	return edits, nil
}

// uniqueAnalyzerName derives a unique ADK sub-agent name from a path. SafeName maps every
// non-alphanumeric character to '_', so distinct paths (e.g. "a/b.kt" and "a-b.kt") can
// collapse to the same name; ParallelAgent requires unique sub-agent names, so a collision
// gets a numeric suffix — otherwise one analyzer silently shadows another and that file's
// edits are dropped. State keys use the full path, so they never collide.
func uniqueAnalyzerName(seen map[string]int, path string) string {
	base := "analyze_" + setup.SafeName(path)
	seen[base]++
	if n := seen[base]; n > 1 {
		return fmt.Sprintf("%s_%d", base, n)
	}
	return base
}

func newAnalyzer(name string, w FileWork, fn EditFunc) (agent.Agent, error) {
	return agent.New(agent.Config{
		Name:        name,
		Description: "Analyzes " + w.Path,
		Run: func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				edit, err := fn(ctx, w)
				if err != nil {
					yield(nil, fmt.Errorf("analyze %s: %w", w.Path, err))
					return
				}
				if edit.Path == "" || strings.TrimSpace(edit.Content) == "" {
					yield(setup.TextEvent(name, "skipped "+w.Path, nil), nil)
					return
				}
				yield(setup.TextEvent(name, "edited "+edit.Path, map[string]any{
					editPrefix + w.Path: edit.Content,
					pathPrefix + w.Path: edit.Path,
				}), nil)
			}
		},
	})
}
