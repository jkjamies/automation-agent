package root

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/ingest"
)

// Deps wires the dispatcher. Each handler is optional. CIResume handles KindCI for
// every fix workflow (lint, coverage) — each engine no-ops unless its check matches.
// SummaryDaily runs the daily commit digest fired by the daily Cloud Scheduler trigger.
type Deps struct {
	SummaryDaily    agent.Agent // KindCronDaily
	LintKickoff     Handler     // KindLint
	CoverageKickoff Handler     // KindCoverage
	CIResume        Handler     // KindCI (dispatched to all fix engines)
	Log             *slog.Logger
}

// BuildRootDispatcher builds the dispatcher and registers the available workflows:
// KindCronDaily → summary; KindLint → lint-fixer; KindCoverage → coverage-fixer;
// KindCI → resume (all fix engines).
func BuildRootDispatcher(d Deps) (*Dispatcher, error) {
	disp := NewDispatcher(d.Log)

	if d.SummaryDaily != nil {
		if err := registerSummary(disp, d.SummaryDaily, ingest.KindCronDaily, "Run the daily commit digest."); err != nil {
			return nil, err
		}
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

// registerSummary builds a runner for a summary agent and registers it under kind,
// driving it with the given trigger text.
func registerSummary(disp *Dispatcher, a agent.Agent, kind ingest.Kind, trigger string) error {
	r, err := setup.NewRunner("automation-agent", a)
	if err != nil {
		return fmt.Errorf("build summary runner: %w", err)
	}
	disp.Register(kind, summaryHandler(r, trigger))
	return nil
}

// summaryHandler drives the summary workflow runner for a cron envelope, using a
// fresh session per fire.
func summaryHandler(r *runner.Runner, trigger string) Handler {
	return func(ctx context.Context, e ingest.Envelope) error {
		sessionID := fmt.Sprintf("summary-%d", e.ReceivedAt.UnixNano())
		return setup.Drive(ctx, r, "system", sessionID, trigger)
	}
}
