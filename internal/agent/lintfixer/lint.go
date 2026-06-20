// Package lintfixer is the lint-remediation configuration of the fixflow engine: it
// supplies a triage step (normalize a linter report) and an analyze step (rewrite
// the affected source files), plus its branch/label/check identity. The loop itself
// lives in internal/agent/fixflow.
package lintfixer

import (
	"embed"

	"github.com/jkjamies/automation-agent/internal/agent/fixflow"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

//go:embed prompts/*.md
var promptFS embed.FS

var prompts = setup.NewPrompts(promptFS)

// NewEngine builds the lint-fixer engine.
func NewEngine(d fixflow.Deps) *fixflow.Engine {
	return fixflow.NewEngine(fixflow.Spec{
		Name:          "lint",
		Branch:        "automation-agent/lint-fix",
		Label:         "automation-agent",
		CheckName:     "agent-lint-verify",
		CommitMessage: "automation-agent: fix lint problems",
		PRTitle:       "automation-agent: fix lint problems",
		SuccessTitle:  "Lint fix succeeded ✅",
		ReviewTitle:   "Lint fix needs human review ⚠️",
		Triage:        Triage,
		Analyze:       Analyze,
	}, d)
}
