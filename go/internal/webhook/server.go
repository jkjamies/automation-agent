// Package webhook exposes the HTTP ingress endpoints. Each request is reduced to a
// normalized ingest.Envelope and handed to an IngestFunc, which should enqueue and
// return quickly. Deterministic tooling — no agent imports.
package webhook

import (
	"context"
	"net/http"
	"time"

	"github.com/jkjamies/automation-agent/internal/ingest"
)

// maxBodyBytes caps how much of a webhook body we read.
const maxBodyBytes = 5 << 20 // 5 MiB

// IngestFunc consumes a normalized envelope. It should enqueue work and return
// quickly; a returned error becomes a 500 to the caller.
type IngestFunc func(ctx context.Context, e ingest.Envelope) error

// Server routes webhook requests to an IngestFunc.
type Server struct {
	ingest IngestFunc
	secret string
	now    func() time.Time
	mux    *http.ServeMux
}

// Option configures a Server.
type Option func(*Server)

// WithGitHubSecret enables HMAC verification of /webhooks/github using secret.
// When empty, verification is skipped (intended for local dev only).
func WithGitHubSecret(secret string) Option {
	return func(s *Server) { s.secret = secret }
}

// WithClock injects a clock for deterministic ReceivedAt timestamps in tests.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// New builds a Server.
func New(ingest IngestFunc, opts ...Option) *Server {
	s := &Server{ingest: ingest, now: time.Now, mux: http.NewServeMux()}
	for _, o := range opts {
		o(s)
	}
	s.routes()
	return s
}

// Handler returns the http.Handler to mount (e.g. on an http.Server).
func (s *Server) Handler() http.Handler { return s.mux }
