// Package harness is the seam between the orchestrator and whatever actually edits code.
//
// The orchestrator never trusts what a harness says it did. It hands the harness a
// checkout and a task, then judges the result itself: it computes the diff from git, runs
// the repository's checks, and applies the diff gates. A harness's own summary is carried
// into the pull request for reviewers, and nothing else.
//
// Two implementations ship:
//   - ADK: an in-process ADK LlmAgent on a Vertex AI model with confined repository tools.
//     Everything stays inside the Google stack and the Go binary.
//   - CLI: any headless coding agent that can be started as a process in the checkout
//     (Antigravity, if and when it offers a headless mode; Gemini CLI; Jules tools; …).
package harness

import (
	"context"
	"time"

	"ticketflow/internal/gate"
	"ticketflow/internal/prompts"
)

// Task is one implementation attempt.
type Task struct {
	Dir              string // checkout to edit, already on the agent branch
	Key              string
	Summary          string
	Description      string
	Plan             string
	TestPlan         string
	RepoInstructions string
	Checks           []gate.Check
	Attempt          int    // 1-based
	Feedback         string // why the previous attempt was rejected; empty on the first
}

// CheckNames lists the task's check names, for prompts.
func (t Task) CheckNames() []string {
	names := make([]string, 0, len(t.Checks))
	for _, c := range t.Checks {
		names = append(names, c.Name)
	}
	return names
}

// Prompt renders the task as the message a coding agent receives.
func (t Task) Prompt() (string, error) { return prompts.Render("task.md", t) }

// Result is what a harness reports. It is informational only.
type Result struct {
	Summary  string
	Elapsed  time.Duration
	Activity int // model calls (ADK) or 0 when unknown
}

// Harness edits the checkout in Task.Dir to implement the task.
type Harness interface {
	Name() string
	Implement(ctx context.Context, t Task) (Result, error)
}
