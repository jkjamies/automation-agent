package tasks

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"automation-agent/internal/ingest"
)

// DefaultMaxConcurrent bounds in-flight in-process dispatches under burst (backpressure).
const DefaultMaxConcurrent = 32

// drainTimeout caps how long Close waits for in-flight in-process dispatches to finish.
const drainTimeout = 15 * time.Second

// InProcess runs each envelope in a background goroutine on a bounded pool — the local-dev
// and default backend. It reproduces the pre-transport behavior exactly: a SIGTERM drains
// in-flight work via Close. It does NOT survive an instance being reclaimed mid-run, which
// is precisely why production uses the Cloud Tasks backend instead. The Name/Delay hints
// are Cloud Tasks features and are ignored here (an immediate, undeduplicated dispatch).
type InProcess struct {
	dispatch DispatchFunc
	log      *slog.Logger
	sem      chan struct{}
	wg       sync.WaitGroup
}

// NewInProcess builds the in-process backend. maxConcurrent < 1 falls back to
// DefaultMaxConcurrent.
func NewInProcess(dispatch DispatchFunc, log *slog.Logger, maxConcurrent int) *InProcess {
	if log == nil {
		log = slog.Default()
	}
	if maxConcurrent < 1 {
		maxConcurrent = DefaultMaxConcurrent
	}
	return &InProcess{dispatch: dispatch, log: log, sem: make(chan struct{}, maxConcurrent)}
}

// Enqueue dispatches e on the bounded pool. It blocks while the pool is full (backpressure
// under burst) and otherwise returns immediately; the dispatch error is logged, not
// returned, because the webhook response has already gone out.
func (p *InProcess) Enqueue(ctx context.Context, e ingest.Envelope, _ ...Option) error {
	select {
	case p.sem <- struct{}{}: // bound concurrency
	case <-ctx.Done():
		return ctx.Err()
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		// A fresh context: the originating webhook request has already returned, so the
		// dispatch must not be cancelled when that request's context is cancelled.
		if err := p.dispatch(context.Background(), e); err != nil {
			p.log.Error("dispatch failed", "kind", e.Kind, "source", e.Source, "err", err)
		}
	}()
	return nil
}

// Close waits (bounded by drainTimeout) for in-flight dispatches to finish so a clean
// SIGTERM completes work in flight rather than abandoning it.
func (p *InProcess) Close() error {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		p.log.Info("drained in-flight work")
	case <-time.After(drainTimeout):
		p.log.Warn("drain timed out; exiting with work still in flight")
	}
	return nil
}
