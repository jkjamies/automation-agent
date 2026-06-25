// Package covfixer is the test-coverage configuration of the fixflow engine: it
// triages an agnostic coverage report into source files with meaningful uncovered
// logic, then generates language-aware tests for them. Its prompts are entirely
// separate from the lint-fixer's; only the deterministic loop is shared (fixflow).
package covfixer

import (
	"embed"

	"automation-agent/internal/agent/fixflow"
	"automation-agent/internal/agent/setup"
)

//go:embed prompts/*.md
var promptFS embed.FS

var prompts = setup.NewPrompts(promptFS)

// NewEngine builds the coverage-fixer engine.
func NewEngine(d fixflow.Deps) *fixflow.Engine {
	return fixflow.NewEngine(fixflow.Spec{
		Name:          "coverage",
		Branch:        "automation-agent/test-coverage",
		CheckName:     "agent-coverage-verify",
		CommitMessage: "automation-agent: add test coverage",
		PRTitle:       "automation-agent: add test coverage",
		SuccessTitle:  "Coverage fix succeeded ✅",
		ReviewTitle:   "Coverage fix needs human review ⚠️",
		Triage:        Triage,
		Analyze:       Analyze,
	}, d)
}
