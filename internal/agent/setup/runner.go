package setup

import (
	"context"

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
