package fixflow

import (
	"context"
	"fmt"

	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"

	"automation-agent/internal/agent/setup"
)

// Explore runs a single tool-using agent that navigates the checkout itself (via
// read_file/list_dir) to examine the real repository — standards docs, existing
// tests, layout — and returns its final text answer. The model decides what to read;
// no Go code pre-selects files. Workflows use this to ground a plan (e.g. where tests
// belong) in the repo's actual conventions rather than a hardcoded rule.
func Explore(ctx context.Context, llm model.LLM, repoDir, instruction, input string) (string, error) {
	tools, err := repoTools(repoDir)
	if err != nil {
		return "", fmt.Errorf("build repo tools: %w", err)
	}
	a, err := llmagent.New(llmagent.Config{
		Name:        "explorer",
		Description: "Examines the repository to ground a plan in its real conventions.",
		Model:       llm,
		Instruction: instruction,
		Tools:       tools,
	})
	if err != nil {
		return "", fmt.Errorf("build explorer: %w", err)
	}
	r, err := setup.NewRunner("fix-explore", a)
	if err != nil {
		return "", err
	}
	return setup.DriveText(ctx, r, "system", "explore", input)
}
