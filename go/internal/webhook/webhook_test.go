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
	"time"

	"automation-agent/internal/ingest"
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
		"X-GitHub-Event":      "check_run",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if c.env.Kind != ingest.KindCI {
		t.Errorf("kind = %q, want ci", c.env.Kind)
	}
}

// A pull_request delivery is the reviewer's native-event kickoff (X-GitHub-Event routes it
// to KindReview, distinct from check_run → KindCI on the same /webhooks/github URL).
func TestGitHubPullRequestRoutesToReview(t *testing.T) {
	c := &capture{}
	s := New(c.ingest)
	body := `{"action":"opened"}`
	rec := do(t, s, http.MethodPost, "/webhooks/github", body, map[string]string{
		"X-GitHub-Event": "pull_request",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if c.env.Kind != ingest.KindReview {
		t.Errorf("kind = %q, want review", c.env.Kind)
	}
	if c.env.Source != "webhook:/github" || string(c.env.Payload) != body {
		t.Errorf("envelope = %+v", c.env)
	}
}

// An event we don't act on (e.g. ping, or one we don't route) is acknowledged with 200 and
// not dispatched, so GitHub records a successful delivery.
func TestGitHubUnroutedEventIsAckedNotDispatched(t *testing.T) {
	for _, event := range []string{"ping", "issues", ""} {
		c := &capture{}
		s := New(c.ingest)
		headers := map[string]string{}
		if event != "" {
			headers["X-GitHub-Event"] = event
		}
		rec := do(t, s, http.MethodPost, "/webhooks/github", `{}`, headers)
		if rec.Code != http.StatusOK {
			t.Errorf("event %q: status = %d, want 200", event, rec.Code)
		}
		if c.env.Kind != "" {
			t.Errorf("event %q: dispatched %q, want no dispatch", event, c.env.Kind)
		}
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
	rec := do(t, s, http.MethodPost, "/webhooks/github", `{}`, map[string]string{
		"X-GitHub-Event": "check_run",
	})
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
	for _, path := range []string{"/internal/cron/daily", "/internal/sweep", "/internal/dispatch"} {
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

// dispatchRecorder captures the envelope passed to a WithDispatch executor and lets a test
// force an error (to assert retry classification).
type dispatchRecorder struct {
	env    ingest.Envelope
	called bool
	err    error
}

func (d *dispatchRecorder) dispatch(_ context.Context, e ingest.Envelope) error {
	d.called = true
	d.env = e
	return d.err
}

// An authorized /internal/dispatch with a valid envelope body executes it in-request (200).
func TestInternalDispatchExecutes(t *testing.T) {
	d := &dispatchRecorder{}
	s := New((&capture{}).ingest, WithInternalToken("secret"), WithDispatch(d.dispatch))
	body, _ := ingest.Encode(ingest.New(ingest.KindCI, "webhook:/github", []byte(`{"x":1}`), time.Unix(0, 0)))
	rec := do(t, s, http.MethodPost, "/internal/dispatch", string(body), map[string]string{"Authorization": "Bearer secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !d.called || d.env.Kind != ingest.KindCI {
		t.Errorf("dispatch not invoked with the decoded envelope: called=%v kind=%q", d.called, d.env.Kind)
	}
}

// /internal/dispatch requires a Bearer credential like the other internal endpoints.
func TestInternalDispatchRequiresBearer(t *testing.T) {
	d := &dispatchRecorder{}
	s := New((&capture{}).ingest, WithInternalToken("secret"), WithDispatch(d.dispatch))
	rec := do(t, s, http.MethodPost, "/internal/dispatch", "{}", map[string]string{"Authorization": "Bearer wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if d.called {
		t.Error("dispatch ran despite a bad token")
	}
}

// With no dispatch executor configured the endpoint reports not-implemented.
func TestInternalDispatchNotConfigured(t *testing.T) {
	s := New((&capture{}).ingest, WithInternalToken("secret"))
	rec := do(t, s, http.MethodPost, "/internal/dispatch", "{}", map[string]string{"Authorization": "Bearer secret"})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

// A poison body (malformed / unknown kind) is acked with 200 so Cloud Tasks drops it
// instead of retrying forever; the dispatch executor is never called (spec §6).
func TestInternalDispatchPoisonBodyIsAcked(t *testing.T) {
	for _, body := range []string{"not json", `{"kind":"jira","source":"x"}`} {
		d := &dispatchRecorder{}
		s := New((&capture{}).ingest, WithInternalToken("secret"), WithDispatch(d.dispatch))
		rec := do(t, s, http.MethodPost, "/internal/dispatch", body, map[string]string{"Authorization": "Bearer secret"})
		if rec.Code != http.StatusOK {
			t.Errorf("body %q: status = %d, want 200 (acked, not retried)", body, rec.Code)
		}
		if d.called {
			t.Errorf("body %q: dispatch ran on a poison payload", body)
		}
	}
}

// A transient dispatch error returns 500 so Cloud Tasks retries with backoff (spec §6).
func TestInternalDispatchTransientErrorIs500(t *testing.T) {
	d := &dispatchRecorder{err: errors.New("llm timeout")}
	s := New((&capture{}).ingest, WithInternalToken("secret"), WithDispatch(d.dispatch))
	body, _ := ingest.Encode(ingest.New(ingest.KindLint, "s", []byte("{}"), time.Unix(0, 0)))
	rec := do(t, s, http.MethodPost, "/internal/dispatch", string(body), map[string]string{"Authorization": "Bearer secret"})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}
