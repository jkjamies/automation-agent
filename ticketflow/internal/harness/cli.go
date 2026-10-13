package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ticketflow/internal/workspace"
)

// CLI runs an external, headless coding agent as a process inside the checkout. Argv may
// contain {prompt_file} (a file holding the rendered task, kept outside the checkout so it
// is never committed), {prompt} (the task text inline), and {workdir}. Examples:
//
//	gemini --approval-mode=yolo --output-format=json -p {prompt}
//	<antigravity headless entrypoint> --workspace {workdir} --task-file {prompt_file}
//
// The process gets a scrubbed environment: PATH, HOME, and only Env. Repository and Jira
// tokens held by the orchestrator are never passed, so whatever the agent runs cannot
// push, merge, or comment; the orchestrator does those after judging the diff.
type CLI struct {
	Argv    []string
	Env     []string // KEY=VALUE pairs, e.g. Vertex AI settings for the agent's model
	Timeout time.Duration
}

// Name implements Harness.
func (h *CLI) Name() string {
	if len(h.Argv) == 0 {
		return "cli"
	}
	return "cli:" + h.Argv[0]
}

// Implement implements Harness.
func (h *CLI) Implement(ctx context.Context, t Task) (Result, error) {
	if len(h.Argv) == 0 {
		return Result{}, fmt.Errorf("harness: cli has no command")
	}
	prompt, err := t.Prompt()
	if err != nil {
		return Result{}, err
	}
	f, err := os.CreateTemp("", "ticketflow-task-*.md")
	if err != nil {
		return Result{}, fmt.Errorf("harness: write task file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		return Result{}, fmt.Errorf("harness: write task file: %w", err)
	}
	if err := f.Close(); err != nil {
		return Result{}, fmt.Errorf("harness: write task file: %w", err)
	}

	r := strings.NewReplacer("{prompt_file}", f.Name(), "{prompt}", prompt, "{workdir}", t.Dir)
	argv := make([]string, len(h.Argv))
	for i, a := range h.Argv {
		argv[i] = r.Replace(a)
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	res, err := workspace.Run(ctx, t.Dir, argv, timeout, h.Env...)
	out := Result{Summary: strings.TrimSpace(res.Output), Elapsed: res.Elapsed}
	switch {
	case err != nil:
		return out, err
	case res.TimedOut:
		return out, fmt.Errorf("harness: %s timed out after %s", h.Name(), timeout)
	case res.ExitCode != 0:
		return out, fmt.Errorf("harness: %s exited %d", h.Name(), res.ExitCode)
	}
	return out, nil
}
