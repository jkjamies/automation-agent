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
	closed   chan struct{} // closed by Close to stop accepting new work
	// mu serializes wg.Add against Close's wg.Wait. Close closes p.closed under mu before
	// waiting, and Enqueue does its closed-recheck + wg.Add under mu, so an Add either
	// happens-before the Wait (and is counted) or is skipped — it can never race the Wait.
	mu        sync.Mutex
	closeOnce sync.Once
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
	return &InProcess{dispatch: dispatch, log: log, sem: make(chan struct{}, maxConcurrent), closed: make(chan struct{})}
}

// Enqueue dispatches e on the bounded pool. It blocks while the pool is full (backpressure
// under burst) and otherwise returns immediately; the dispatch error is logged, not
// returned, because the webhook response has already gone out.
func (p *InProcess) Enqueue(ctx context.Context, e ingest.Envelope, _ ...Option) error {
	select {
	case <-p.closed:
		// Shutdown has begun: refuse new work rather than launch a goroutine that Close has
		// already stopped waiting for (it would be abandoned on exit, and a wg.Add racing the
		// concurrent wg.Wait in Close is itself unsafe).
		return context.Canceled
	default:
	}
	select {
	case <-p.closed:
		return context.Canceled
	case p.sem <- struct{}{}: // bound concurrency
	case <-ctx.Done():
		return ctx.Err()
	}
	// Register on the WaitGroup under the lock, and only if Close has not begun. Taking a slot
	// above can happen at the same instant Close runs (the select picks randomly when both
	// p.closed and the slot are ready), so without this guard wg.Add could race wg.Wait. Close
	// closes p.closed under the same lock before waiting, so here we either Add before that
	// (counted by Wait) or observe closed and back out, releasing the slot.
	p.mu.Lock()
	select {
	case <-p.closed:
		p.mu.Unlock()
		<-p.sem // release the slot we just took
		return context.Canceled
	default:
	}
	p.wg.Add(1)
	p.mu.Unlock()
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
	// Stop accepting new work before waiting, so Enqueue cannot launch a goroutine the drain
	// would miss. Closing under mu orders this against Enqueue's under-lock wg.Add, so the
	// wg.Wait below can never race a concurrent wg.Add.
	p.closeOnce.Do(func() {
		p.mu.Lock()
		close(p.closed)
		p.mu.Unlock()
	})
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
