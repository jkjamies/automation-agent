package obs

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPMiddlewareOneSpanPerRequest(t *testing.T) {
	exp := installRecording(t)

	var served int
	h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusAccepted)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/lint", nil))

	if served != 1 {
		t.Fatalf("wrapped handler ran %d times, want 1", served)
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (middleware must not alter the response)", rec.Code)
	}
	// The middleware flushes after the handler returns, so the server span is already
	// exported by the time ServeHTTP returns — the in-request flush guard at the HTTP layer.
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly one server span exported in-request, got %d", len(spans))
	}
	if spans[0].Name != "POST /webhooks/lint" {
		t.Errorf("server span name = %q, want %q", spans[0].Name, "POST /webhooks/lint")
	}
}

func TestHTTPMiddlewareExcludesHealth(t *testing.T) {
	exp := installRecording(t)

	h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, healthPath, nil))

	if got := len(exp.GetSpans()); got != 0 {
		t.Errorf("health probe produced %d spans, want 0 (it must be excluded)", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", rec.Code)
	}
}
