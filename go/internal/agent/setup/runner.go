package setup

import (
	"context"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
)

// NewRunner builds an in-memory runner rooted at root, suitable for driving a
// workflow agent to completion locally and in tests.
func NewRunner(appName string, root agent.Agent) (*runner.Runner, error) {
	return runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
}

// Drive runs the agent for a single input, draining events and returning the first
// error. Side-effecting agents (e.g. a notifier) perform their work as they run.
func Drive(ctx context.Context, r *runner.Runner, userID, sessionID, input string) error {
	for _, err := range r.Run(ctx, userID, sessionID, UserText(input), agent.RunConfig{}) {
		if err != nil {
			return err
		}
	}
	return nil
}

// DriveText runs the agent and returns the concatenated text of its non-partial
// responses. For a tool-using agent this is the final answer after any tool calls
// (intermediate function-call/response events carry no text).
func DriveText(ctx context.Context, r *runner.Runner, userID, sessionID, input string) (string, error) {
	var sb strings.Builder
	for ev, err := range r.Run(ctx, userID, sessionID, UserText(input), agent.RunConfig{}) {
		if err != nil {
			return "", err
		}
		if ev.Content != nil && !ev.Partial {
			sb.WriteString(contentText(ev.Content))
		}
	}
	return sb.String(), nil
}

// DriveCollectState runs the agent and accumulates every state delta emitted by its
// events into a single map. Useful for fan-out workflows where parallel sub-agents
// each write a distinct state key the caller needs to read back.
func DriveCollectState(ctx context.Context, r *runner.Runner, userID, sessionID, input string) (map[string]any, error) {
	state := make(map[string]any)
	for ev, err := range r.Run(ctx, userID, sessionID, UserText(input), agent.RunConfig{}) {
		if err != nil {
			return nil, err
		}
		for k, v := range ev.Actions.StateDelta {
			state[k] = v
		}
	}
	return state, nil
}
