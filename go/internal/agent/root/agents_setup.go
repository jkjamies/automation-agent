package root

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/adk/v2/agent"

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
	ReviewKickoff   Handler     // KindReview (PR code-review agent)
	Log             *slog.Logger
}

// BuildRootDispatcher builds the dispatcher and registers the available workflows:
// KindCronDaily → summary; KindLint → lint-fixer; KindCoverage → coverage-fixer;
// KindCI → resume (all fix engines); KindReview → PR code-review agent.
func BuildRootDispatcher(d Deps) (*Dispatcher, error) {
	disp := newDispatcher(d.Log)

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
	if d.ReviewKickoff != nil {
		disp.Register(ingest.KindReview, d.ReviewKickoff)
	}
	return disp, nil
}

const (
	// summaryApp is the ADK app name the summary runner is built under.
	summaryApp = "automation-agent"
	// summarySession is the session every fire drives. A constant is safe — and better than a
	// per-fire unique id — precisely because the runner is per-fire: each fire brings its own
	// session service, so two concurrent fires cannot collide. It also turns the retention bug
	// below into something a test can see: were the runner ever hoisted back out to startup,
	// the second fire would inherit the first's events instead of leaking them silently.
	summarySession = "summary"
)

// registerSummary registers the summary agent under kind, driven by the given trigger text.
//
// A runner is built here and thrown away purely to fail fast: construction validates the agent
// tree (duplicate sub-agent names, for one), and a misbuilt summary agent should stop startup
// rather than first surface on a cron fire hours later. The runner each fire actually uses is
// built by the handler.
func registerSummary(disp *Dispatcher, a agent.Agent, kind ingest.Kind, trigger string) error {
	if _, err := setup.NewRunner(summaryApp, a); err != nil {
		return fmt.Errorf("build summary runner: %w", err)
	}
	disp.Register(kind, summaryHandler(a, trigger))
	return nil
}

// summaryHandler drives the summary workflow for a cron envelope, building its runner per fire.
//
// Per fire, not once at registration, because the runner owns the in-memory session service
// behind it. A runner retained for the process lifetime retains every session ever driven
// through it, and nothing deletes them — each daily fire would strand a session holding the
// commit digest for every configured repo plus the model's output, for as long as the instance
// lives. Building it here lets the runner, its session service, and that fire's session all
// become garbage together, and matches every other runner call site in the codebase, which are
// likewise build-and-discard.
func summaryHandler(a agent.Agent, trigger string) Handler {
	return func(ctx context.Context, _ ingest.Envelope) error {
		r, err := setup.NewRunner(summaryApp, a)
		if err != nil {
			return fmt.Errorf("build summary runner: %w", err)
		}
		return setup.Drive(ctx, r, "system", summarySession, trigger)
	}
}
