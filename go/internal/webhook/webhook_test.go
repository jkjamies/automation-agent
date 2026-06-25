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

func TestCoverageKickoff(t *testing.T) {
	c := &capture{}
	s := New(c.ingest)
	rec := do(t, s, http.MethodPost, "/webhooks/coverage", `{"report":"jacoco"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if c.env.Kind != ingest.KindCoverage {
		t.Errorf("kind = %q, want coverage", c.env.Kind)
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

// A body larger than the cap is rejected with 413 rather than silently truncated.
func TestOversizeBodyIsRejected(t *testing.T) {
	c := &capture{}
	s := New(c.ingest)
	oversize := strings.Repeat("x", maxBodyBytes+100)
	rec := do(t, s, http.MethodPost, "/webhooks/lint", oversize, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if c.env.Payload != nil {
		t.Errorf("oversize body should not be dispatched, got %d bytes", len(c.env.Payload))
	}
}

// Kickoff endpoints select a caller-supplied repo, so they require the HMAC signature
// when a secret is configured.
func TestLintKickoffRequiresSignature(t *testing.T) {
	c := &capture{}
	s := New(c.ingest, WithGitHubSecret("topsecret"))
	body := `{"problems":[]}`

	rec := do(t, s, http.MethodPost, "/webhooks/lint", body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status = %d, want 401", rec.Code)
	}

	rec = do(t, s, http.MethodPost, "/webhooks/lint", body, map[string]string{
		"X-Hub-Signature-256": sign("topsecret", body),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("signed status = %d, want 202", rec.Code)
	}
}

func TestCoverageKickoffRequiresSignature(t *testing.T) {
	c := &capture{}
	s := New(c.ingest, WithGitHubSecret("topsecret"))
	rec := do(t, s, http.MethodPost, "/webhooks/coverage", `{"report":"jacoco"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status = %d, want 401", rec.Code)
	}
}

// With no INTERNAL_TOKEN, the /internal/* endpoints are disabled (404).
func TestInternalEndpointsDisabledWithoutToken(t *testing.T) {
	c := &capture{}
	s := New(c.ingest)
	for _, path := range []string{"/internal/cron/daily", "/internal/sweep"} {
		if rec := do(t, s, http.MethodPost, path, "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s without token = %d, want 404", path, rec.Code)
		}
	}
}

// A configured token requires a matching Bearer credential.
func TestInternalRequiresBearer(t *testing.T) {
	c := &capture{}
	s := New(c.ingest, WithInternalToken("secret"))
	if rec := do(t, s, http.MethodPost, "/internal/cron/daily", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no bearer = %d, want 401", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/internal/cron/daily", "", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer = %d, want 401", rec.Code)
	}
}

// An authorized cron call dispatches the corresponding summary envelope.
func TestInternalCronDispatches(t *testing.T) {
	c := &capture{}
	s := New(c.ingest, WithInternalToken("secret"))
	rec := do(t, s, http.MethodPost, "/internal/cron/daily", "", map[string]string{"Authorization": "Bearer secret"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if c.env.Kind != ingest.KindCronDaily {
		t.Errorf("kind = %q, want cron.daily", c.env.Kind)
	}
}

// An authorized sweep call invokes the sweep function.
func TestInternalSweep(t *testing.T) {
	c := &capture{}
	swept := false
	s := New(c.ingest, WithInternalToken("secret"), WithSweep(func(context.Context) error { swept = true; return nil }))
	rec := do(t, s, http.MethodPost, "/internal/sweep", "", map[string]string{"Authorization": "Bearer secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !swept {
		t.Error("sweep func was not invoked")
	}
}

// Sweep with no sweep function configured reports not-implemented.
func TestInternalSweepNotConfigured(t *testing.T) {
	c := &capture{}
	s := New(c.ingest, WithInternalToken("secret"))
	rec := do(t, s, http.MethodPost, "/internal/sweep", "", map[string]string{"Authorization": "Bearer secret"})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}
