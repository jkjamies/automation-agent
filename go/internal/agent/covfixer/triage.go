package covfixer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/adk/model"

	"automation-agent/internal/agent/fixflow"
	"automation-agent/internal/agent/setup"
)

// Triage uses the LLM to normalize an arbitrary coverage report into the source
// files with meaningful uncovered logic.
func Triage(ctx context.Context, llm model.LLM, report string) ([]fixflow.FileWork, error) {
	out, err := setup.GenerateText(ctx, llm, prompts.MustGet("triage"), report)
	if err != nil {
		return nil, fmt.Errorf("triage: %w", err)
	}
	work, err := parseTriage(out)
	if err != nil {
		return nil, fmt.Errorf("triage: %w", err)
	}
	if len(work) == 0 {
		return nil, fmt.Errorf("triage: no meaningful uncovered files found in report")
	}
	return work, nil
}

type triageFile struct {
	Path      string   `json:"path"`
	Uncovered []string `json:"uncovered"`
}

func parseTriage(out string) ([]fixflow.FileWork, error) {
	js := fixflow.ExtractJSONArray(out)
	if js == "" {
		return nil, fmt.Errorf("no JSON array in model output")
	}
	var files []triageFile
	if err := json.Unmarshal([]byte(js), &files); err != nil {
		return nil, fmt.Errorf("decode triage JSON: %w", err)
	}
	work := make([]fixflow.FileWork, 0, len(files))
	for _, f := range files {
		if strings.TrimSpace(f.Path) != "" {
			work = append(work, fixflow.FileWork{Path: f.Path, Items: f.Uncovered})
		}
	}
	return work, nil
}
