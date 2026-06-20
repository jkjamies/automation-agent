package root

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"

	"github.com/jkjamies/automation-agent/internal/agent/setup"
	"github.com/jkjamies/automation-agent/internal/ingest"
)

// Deps wires the dispatcher. SummaryAgent may be nil to leave the summary workflow
// disabled (e.g. when no repos are configured).
type Deps struct {
	SummaryAgent agent.Agent
	Log          *slog.Logger
}

// BuildRootDispatcher builds the dispatcher and registers the workflows that are
// available. The cron kinds route to the summary workflow.
func BuildRootDispatcher(d Deps) (*Dispatcher, error) {
	disp := NewDispatcher(d.Log)

	if d.SummaryAgent != nil {
		r, err := setup.NewRunner("automation-agent", d.SummaryAgent)
		if err != nil {
			return nil, fmt.Errorf("build summary runner: %w", err)
		}
		h := summaryHandler(r)
		disp.Register(ingest.KindCronDaily, h)
		disp.Register(ingest.KindCronWeekly, h)
	}

	// KindLint / KindCI are registered by the lint-fixer in a later phase.
	return disp, nil
}

// summaryHandler drives the summary workflow runner for a cron envelope, using a
// fresh session per fire.
func summaryHandler(r *runner.Runner) Handler {
	return func(ctx context.Context, e ingest.Envelope) error {
		sessionID := fmt.Sprintf("summary-%d", e.ReceivedAt.UnixNano())
		return setup.Drive(ctx, r, "system", sessionID, "Run the daily commit digest.")
	}
}
