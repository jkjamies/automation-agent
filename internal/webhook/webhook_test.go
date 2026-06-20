package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jkjamies/automation-agent/internal/ingest"
)

type capture struct {
	env ingest.Envelope
	err error
}

func (c *capture) ingest(_ context.Context, e ingest.Envelope) error {
	c.env = e
	return c.err
}

func do(t *testing.T, s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestLintKickoff(t *testing.T) {
	c := &capture{}
	s := New(c.ingest)
	rec := do(t, s, http.MethodPost, "/webhooks/lint", `{"problems":[]}`, nil)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if c.env.Kind != ingest.KindLint {
		t.Errorf("kind = %q, want lint", c.env.Kind)
	}
	if string(c.env.Payload) != `{"problems":[]}` {
		t.Errorf("payload = %s", c.env.Payload)
	}
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubSignatureValid(t *testing.T) {
	c := &capture{}
	s := New(c.ingest, WithGitHubSecret("topsecret"))
	body := `{"action":"completed"}`
	rec := do(t, s, http.MethodPost, "/webhooks/github", body, map[string]string{
		"X-Hub-Signature-256": sign("topsecret", body),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if c.env.Kind != ingest.KindCI {
		t.Errorf("kind = %q, want ci", c.env.Kind)
	}
}

func TestGitHubSignatureInvalid(t *testing.T) {
	c := &capture{}
	s := New(c.ingest, WithGitHubSecret("topsecret"))
	rec := do(t, s, http.MethodPost, "/webhooks/github", `{}`, map[string]string{
		"X-Hub-Signature-256": "sha256=deadbeef",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGitHubNoSecretSkipsVerification(t *testing.T) {
	c := &capture{}
	s := New(c.ingest) // no secret
	rec := do(t, s, http.MethodPost, "/webhooks/github", `{}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

func TestIngestErrorIs500(t *testing.T) {
	c := &capture{err: errors.New("boom")}
	s := New(c.ingest)
	rec := do(t, s, http.MethodPost, "/webhooks/lint", `{}`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	rec := do(t, New((&capture{}).ingest), http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("health = %d %q", rec.Code, rec.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rec := do(t, New((&capture{}).ingest), http.MethodGet, "/webhooks/lint", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
