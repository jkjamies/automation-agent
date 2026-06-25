// Package root is the dispatcher kicked off for every ingest. It routes a
// normalized ingest.Envelope to the right workflow by Kind. Keeping a single entry
// point is why "root" exists: new ingress sources (GitHub/Jira/Confluence/human)
// and smarter (e.g. LLM-based) routing slot in here without restructuring.
package root

import (
	"context"
	"log/slog"

	"automation-agent/internal/ingest"
)

// Handler runs the work for one ingest envelope.
type Handler func(ctx context.Context, e ingest.Envelope) error

// Dispatcher routes envelopes to handlers by Kind.
type Dispatcher struct {
	handlers map[ingest.Kind]Handler
	log      *slog.Logger
}

// NewDispatcher creates an empty dispatcher.
func NewDispatcher(log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{handlers: make(map[ingest.Kind]Handler), log: log}
}

// Register binds a handler to a kind (last registration wins).
func (d *Dispatcher) Register(kind ingest.Kind, h Handler) {
	d.handlers[kind] = h
}

// Handles reports whether a kind has a registered handler.
func (d *Dispatcher) Handles(kind ingest.Kind) bool {
	_, ok := d.handlers[kind]
	return ok
}

// Dispatch routes one envelope. An unregistered kind is logged and ignored, so an
// ingress that isn't wired yet (e.g. lint before Phase 5) is a no-op, not a crash.
func (d *Dispatcher) Dispatch(ctx context.Context, e ingest.Envelope) error {
	h, ok := d.handlers[e.Kind]
	if !ok {
		d.log.Warn("no handler for ingest kind", "kind", e.Kind, "source", e.Source)
		return nil
	}
	d.log.Info("dispatching", "kind", e.Kind, "source", e.Source)
	return h(ctx, e)
}
