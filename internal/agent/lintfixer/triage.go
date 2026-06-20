package lintfixer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/agent/lintfixer/models"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

// Triage uses the LLM to normalize an arbitrary linter report into a list of files
// and their problems, so the lint-fixer is agnostic to the reporting format. We
// constrain only the model's output shape, never the input.
func Triage(ctx context.Context, llm model.LLM, report string) ([]models.FileProblems, error) {
	out, err := setup.GenerateText(ctx, llm, prompts.MustGet("triage"), report)
	if err != nil {
		return nil, fmt.Errorf("triage: %w", err)
	}
	files, err := parseTriage(out)
	if err != nil {
		return nil, fmt.Errorf("triage: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("triage: no actionable files found in report")
	}
	return files, nil
}

// parseTriage extracts the JSON array of {path, problems} from model output,
// tolerating surrounding prose or code fences.
func parseTriage(out string) ([]models.FileProblems, error) {
	js := extractJSONArray(out)
	if js == "" {
		return nil, fmt.Errorf("no JSON array in model output")
	}
	var files []models.FileProblems
	if err := json.Unmarshal([]byte(js), &files); err != nil {
		return nil, fmt.Errorf("decode triage JSON: %w", err)
	}
	kept := files[:0]
	for _, f := range files {
		if strings.TrimSpace(f.Path) != "" {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// extractJSONArray returns the substring from the first '[' to the last ']'.
func extractJSONArray(s string) string {
	i := strings.IndexByte(s, '[')
	j := strings.LastIndexByte(s, ']')
	if i < 0 || j < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}
