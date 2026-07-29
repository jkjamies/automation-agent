// Package webhook exposes the HTTP ingress endpoints. Each request is reduced to a
// normalized ingest.Envelope and handed to an IngestFunc, which should enqueue and
// return quickly. Deterministic tooling — no agent imports.
package webhook

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"automation-agent/internal/ingest"
)

// Body caps, per route class. They differ because the two classes carry different things:
// an ingress route receives a raw source body that still has to be wrapped and base64'd
// into a task, while /internal/dispatch receives that already-encoded envelope.
const (
	// maxIngressBytes caps a raw source body (/webhooks/*, /internal/cron/*). It is the
	// largest body that is still enqueueable after encoding, so anything accepted here can
	// actually be dispatched. A larger body is rejected with 413 — permanent and honest —
	// instead of being accepted and then failing at enqueue with a retryable 500 that the
	// caller would retry forever against a body that can never fit.
	maxIngressBytes = ingest.MaxPayloadBytes

	// maxDispatchBytes caps the Cloud Tasks worker body, which is a wire envelope already
	// grown by base64. Capping it at maxIngressBytes would reject every task at the top of
	// the accepted size range.
	maxDispatchBytes = ingest.MaxEncodedBytes
)

// IngestFunc consumes a normalized envelope. It should enqueue work and return
// quickly; a returned error becomes a 500 to the caller.
type IngestFunc func(ctx context.Context, e ingest.Envelope) error

// SweepFunc runs the durable housekeeping passes: resolving parked runs whose CI never
// reported (the timeout catch-all) and reaping runs nothing can ever resolve. Driven by
// Cloud Scheduler via POST /internal/sweep.
type SweepFunc func(ctx context.Context) error

// DispatchFunc runs an envelope's workflow synchronously, in-request. It backs
// POST /internal/dispatch, which the Cloud Tasks transport delivers to so the compute
// runs on allocated CPU (unlike a post-202 background goroutine). It is
// root.Dispatcher.Dispatch.
type DispatchFunc func(ctx context.Context, e ingest.Envelope) error

// Server routes webhook requests to an IngestFunc.
type Server struct {
	ingest        IngestFunc
	secret        string
	internalToken string
	sweep         SweepFunc
	dispatchFn    DispatchFunc
	now           func() time.Time
	log           *slog.Logger
	mux           *http.ServeMux
}

// Option configures a Server.
type Option func(*Server)

// WithGitHubSecret enables HMAC verification of /webhooks/github using secret.
// When empty, verification is skipped (intended for local dev only).
func WithGitHubSecret(secret string) Option {
	return func(s *Server) { s.secret = secret }
}

// WithInternalToken enables the /internal/* endpoints (cron + sweep), authenticated with a
// Bearer token (Cloud Scheduler attaches it). When empty, those endpoints return 404 — they
// are disabled unless explicitly configured.
func WithInternalToken(token string) Option {
	return func(s *Server) { s.internalToken = token }
}

// WithSweep wires the sweep function invoked by POST /internal/sweep.
func WithSweep(fn SweepFunc) Option {
	return func(s *Server) { s.sweep = fn }
}

// WithDispatch wires the synchronous, in-request executor invoked by POST /internal/dispatch
// (the Cloud Tasks transport's worker endpoint). When unset, that endpoint returns 501.
func WithDispatch(fn DispatchFunc) Option {
	return func(s *Server) { s.dispatchFn = fn }
}

// WithLogger sets the logger used for non-fatal handler diagnostics (e.g. a poison
// /internal/dispatch body that is acked rather than retried). A nil logger is ignored so
// the non-nil default (slog.Default) is preserved — handleDispatch always has a logger.
func WithLogger(log *slog.Logger) Option {
	return func(s *Server) {
		if log != nil {
			s.log = log
		}
	}
}

// New builds a Server.
func New(enqueue IngestFunc, opts ...Option) *Server {
	s := &Server{ingest: enqueue, now: time.Now, log: slog.Default(), mux: http.NewServeMux()}
	for _, o := range opts {
		o(s)
	}
	s.routes()
	return s
}

// Handler returns the http.Handler to mount (e.g. on an http.Server).
func (s *Server) Handler() http.Handler { return s.mux }
