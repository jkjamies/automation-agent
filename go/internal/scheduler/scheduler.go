// Package scheduler turns cron schedules into ingest envelopes. Each fire emits a
// normalized ingest.Envelope so the root agent treats time-based triggers exactly
// like any other ingress. Deterministic tooling — no agent imports.
package scheduler

import (
	"context"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/jkjamies/automation-agent/internal/ingest"
)

// EmitFunc receives an envelope when a schedule fires.
type EmitFunc func(ingest.Envelope)

// Scheduler registers cron specs that emit ingest envelopes.
type Scheduler struct {
	cron *cron.Cron
	emit EmitFunc
	now  func() time.Time
}

// New builds a Scheduler that calls emit on each fire. Schedules are interpreted in
// UTC (not the host's local zone) so "0 9 * * *" means 09:00 UTC regardless of where
// the process runs.
func New(emit EmitFunc) *Scheduler {
	return &Scheduler{cron: cron.New(cron.WithLocation(time.UTC)), emit: emit, now: time.Now}
}

// Add registers a 5-field cron spec (minute hour dom month dow) that emits an
// envelope of the given kind. It returns an error for an invalid spec.
func (s *Scheduler) Add(spec string, kind ingest.Kind) error {
	_, err := s.cron.AddFunc(spec, func() { s.trigger(kind) })
	return err
}

// trigger emits one envelope; separated from the cron closure so it is directly
// unit-testable without waiting for a real schedule.
func (s *Scheduler) trigger(kind ingest.Kind) {
	s.emit(ingest.New(kind, "scheduler", nil, s.now()))
}

// Start begins the cron loop (non-blocking).
func (s *Scheduler) Start() { s.cron.Start() }

// Stop halts scheduling and returns a context that is done once running jobs end.
func (s *Scheduler) Stop() context.Context { return s.cron.Stop() }

// Entries reports the number of registered schedules (useful for assertions).
func (s *Scheduler) Entries() int { return len(s.cron.Entries()) }
