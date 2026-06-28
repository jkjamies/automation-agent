// Package tasks is the execution transport between webhook ingress and the dispatcher.
// Webhook ingress reduces a request to an ingest.Envelope and calls Transport.Enqueue,
// which returns fast; the envelope's workflow runs later — in a background goroutine for
// the in-process backend, or in a fresh /internal/dispatch request delivered by Cloud
// Tasks in production. The seam exists because on Cloud Run with request-based billing
// CPU is throttled to near-zero once a response is sent, so multi-minute LLM compute must
// run *inside* a request (Cloud Tasks gives that, plus durable retry and rate limiting).
// See specs/20260626-workflow-execution-transport.md. Deterministic tooling — no agent
// imports (the dispatcher is injected as a DispatchFunc).
package tasks

import (
	"context"
	"time"

	"automation-agent/internal/ingest"
)

// DispatchFunc runs the work for one envelope. It is root.Dispatcher.Dispatch, passed in
// so this package stays decoupled from the agent layer.
type DispatchFunc func(ctx context.Context, e ingest.Envelope) error

// Transport enqueues an envelope for asynchronous execution and returns quickly. A
// returned error becomes a 500 to the webhook caller (so GitHub/Cloud Scheduler retries).
type Transport interface {
	// Enqueue schedules e for execution. opts carry optional, backend-honored hints.
	Enqueue(ctx context.Context, e ingest.Envelope, opts ...Option) error
	// Close releases the backend: the in-process backend drains in-flight goroutines; the
	// Cloud Tasks backend closes its gRPC client. Safe to call once at shutdown.
	Close() error
}

// Options are optional per-enqueue hints. The transport stays deliberately dumb about
// workflow semantics: it carries these to the backend but does not interpret them.
// Coalesce-to-latest / staleness logic lives in the workflow, not here (spec Decision §3).
type Options struct {
	// Name is a dedup key. Cloud Tasks drops a duplicate task with the same name for ~1h,
	// giving idempotency against a redelivered webhook. Empty means no dedup.
	Name string
	// Delay schedules delivery this far in the future (e.g. a review debounce window).
	// Zero means deliver immediately. Only the Cloud Tasks backend honors it.
	Delay time.Duration
}

// Option mutates Options.
type Option func(*Options)

// WithName sets the dedup task name (see Options.Name).
func WithName(name string) Option { return func(o *Options) { o.Name = name } }

// WithDelay sets the schedule delay (see Options.Delay).
func WithDelay(d time.Duration) Option { return func(o *Options) { o.Delay = d } }

// apply folds the option funcs into an Options value.
func apply(opts []Option) Options {
	var o Options
	for _, fn := range opts {
		fn(&o)
	}
	return o
}
