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

// Deps wires the dispatcher. Each handler is optional. CIResume handles KindCI for
// every fix workflow (lint, coverage) — each engine no-ops unless its check matches.
type Deps struct {
	SummaryAgent    agent.Agent
	LintKickoff     Handler // KindLint
	CoverageKickoff Handler // KindCoverage
	CIResume        Handler // KindCI (dispatched to all fix engines)
	Log             *slog.Logger
}

// BuildRootDispatcher builds the dispatcher and registers the available workflows:
// cron kinds → summary; KindLint → lint-fixer; KindCoverage → coverage-fixer;
// KindCI → resume (all fix engines).
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
	if d.LintKickoff != nil {
		disp.Register(ingest.KindLint, d.LintKickoff)
	}
	if d.CoverageKickoff != nil {
		disp.Register(ingest.KindCoverage, d.CoverageKickoff)
	}
	if d.CIResume != nil {
		disp.Register(ingest.KindCI, d.CIResume)
	}
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
