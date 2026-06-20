package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/jkjamies/automation-agent/internal/ingest"
)

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /webhooks/lint", s.handleLint)
	s.mux.HandleFunc("POST /webhooks/github", s.handleGitHub)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// handleLint is the lint-fixer kickoff: an agnostic lint JSON payload.
func (s *Server) handleLint(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	s.dispatch(w, r.Context(), ingest.New(ingest.KindLint, "webhook:/lint", body, s.now()))
}

// handleGitHub is the lint-fixer resume: GitHub check_run events.
func (s *Server) handleGitHub(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if s.secret != "" && !verifySignature(s.secret, r.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	s.dispatch(w, r.Context(), ingest.New(ingest.KindCI, "webhook:/github", body, s.now()))
}

func (s *Server) dispatch(w http.ResponseWriter, ctx context.Context, e ingest.Envelope) {
	if err := s.ingest(ctx, e); err != nil {
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
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
