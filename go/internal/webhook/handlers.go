package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jkjamies/automation-agent/internal/ingest"
)

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /webhooks/lint", s.handleLint)
	s.mux.HandleFunc("POST /webhooks/coverage", s.handleCoverage)
	s.mux.HandleFunc("POST /webhooks/github", s.handleGitHub)
	// Cloud Scheduler ingress (Bearer-token auth; disabled unless INTERNAL_TOKEN is set).
	s.mux.HandleFunc("POST /internal/cron/daily", s.handleCronDaily)
	s.mux.HandleFunc("POST /internal/cron/weekly", s.handleCronWeekly)
	s.mux.HandleFunc("POST /internal/sweep", s.handleSweep)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// handleLint is the lint-fixer kickoff: an agnostic lint JSON payload. A kickoff selects
// the caller-supplied target repo, so it is HMAC-authenticated with the same shared
// secret as the GitHub webhook (verification is skipped only when no secret is set).
func (s *Server) handleLint(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r)
	if !ok || !s.authenticated(w, r, body) {
		return
	}
	s.dispatch(r.Context(), w, ingest.New(ingest.KindLint, "webhook:/lint", body, s.now()))
}

// handleCoverage is the coverage-fixer kickoff: an agnostic coverage report. Like the
// lint kickoff it is HMAC-authenticated when a secret is configured.
func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r)
	if !ok || !s.authenticated(w, r, body) {
		return
	}
	s.dispatch(r.Context(), w, ingest.New(ingest.KindCoverage, "webhook:/coverage", body, s.now()))
}

// handleGitHub is the lint/coverage-fixer resume: GitHub check_run events.
func (s *Server) handleGitHub(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r)
	if !ok || !s.authenticated(w, r, body) {
		return
	}
	s.dispatch(r.Context(), w, ingest.New(ingest.KindCI, "webhook:/github", body, s.now()))
}

// handleCronDaily / handleCronWeekly let Cloud Scheduler trigger the summary digests, so
// the cron schedule lives GCP-side and the Cloud Run instance can scale to zero between
// fires. They emit the same envelopes the in-process scheduler would.
func (s *Server) handleCronDaily(w http.ResponseWriter, r *http.Request) {
	if !s.internalAuthenticated(w, r) {
		return
	}
	s.dispatch(r.Context(), w, ingest.New(ingest.KindCronDaily, "internal:/cron/daily", nil, s.now()))
}

func (s *Server) handleCronWeekly(w http.ResponseWriter, r *http.Request) {
	if !s.internalAuthenticated(w, r) {
		return
	}
	s.dispatch(r.Context(), w, ingest.New(ingest.KindCronWeekly, "internal:/cron/weekly", nil, s.now()))
}

// handleSweep runs the durable timeout sweep (Cloud Scheduler drives it on a schedule),
// resolving parked runs whose CI never reported.
func (s *Server) handleSweep(w http.ResponseWriter, r *http.Request) {
	if !s.internalAuthenticated(w, r) {
		return
	}
	if s.sweep == nil {
		http.Error(w, "sweep not configured", http.StatusNotImplemented)
		return
	}
	if err := s.sweep(r.Context()); err != nil {
		http.Error(w, "sweep failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// internalAuthenticated guards the /internal/* endpoints with a Bearer token. With no token
// configured the endpoints are disabled (404), so they are never open by default.
func (s *Server) internalAuthenticated(w http.ResponseWriter, r *http.Request) bool {
	if s.internalToken == "" {
		http.Error(w, "internal endpoints disabled", http.StatusNotFound)
		return false
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	got := strings.TrimPrefix(auth, prefix)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.internalToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// authenticated verifies the request's HMAC signature when a secret is configured,
// writing a 401 and returning false on mismatch. With no secret (local dev only) every
// request passes.
func (s *Server) authenticated(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if s.secret == "" {
		return true
	}
	if !verifySignature(s.secret, r.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) dispatch(ctx context.Context, w http.ResponseWriter, e ingest.Envelope) {
	if err := s.ingest(ctx, e); err != nil {
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// readBody reads up to maxBodyBytes. A body over the cap is rejected with 413 rather
// than silently truncated — a truncated body would both fail HMAC verification and feed
// malformed JSON downstream. Returns false (after writing the error response) on failure.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "read body", http.StatusBadRequest)
		}
		return nil, false
	}
	return body, true
}

// verifySignature checks a GitHub "sha256=<hex>" HMAC over the request body.
func verifySignature(secret, header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.TrimPrefix(header, prefix)))
}
