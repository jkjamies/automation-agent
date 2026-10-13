package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ticketflow/internal/jira"
	"ticketflow/internal/pipeline"
)

type fakeRunner struct {
	mu      sync.Mutex
	keys    []string
	release chan struct{}
	err     error
}

func (f *fakeRunner) Run(_ context.Context, key string, _ pipeline.RunOptions) (pipeline.Outcome, error) {
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	f.keys = append(f.keys, key)
	f.mu.Unlock()
	return pipeline.Outcome{Status: pipeline.StatusPublished, Key: key}, f.err
}

func (f *fakeRunner) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

const secret = "s3cret"

func post(t *testing.T, h http.Handler, body string, token string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/jira", strings.NewReader(body))
	if token != "" {
		req.Header.Set(jira.TokenHeader, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func issueEvent(key string, labels ...string) string {
	return `{"webhookEvent":"jira:issue_updated","issue":{"key":"` + key + `","fields":{"labels":["` + strings.Join(labels, `","`) + `"]}}}`
}

func newServer(t *testing.T, r Runner, queue int) *Server {
	t.Helper()
	s, err := New(Options{Secret: secret, TriggerLabel: "agent-ready", Runner: r, QueueSize: queue, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWebhookRouting(t *testing.T) {
	r := &fakeRunner{}
	s := newServer(t, r, 4)
	h := s.Handler()

	// A slice, not a map: the duplicate case depends on the trigger case before it.
	for _, tc := range []struct {
		name, body, token string
		code              int
		want              string
	}{
		{"unauthenticated", issueEvent("P-1", "agent-ready"), "wrong", http.StatusUnauthorized, "unauthenticated"},
		{"not an issue", `{"webhookEvent":"sprint_started"}`, secret, http.StatusOK, "ignored"},
		{"bad json", `{`, secret, http.StatusBadRequest, "invalid"},
		{"no trigger", issueEvent("P-1", "other"), secret, http.StatusOK, "ignored"},
		{"trigger", issueEvent("P-1", "agent-ready"), secret, http.StatusAccepted, "queued"},
		{"duplicate", issueEvent("P-1", "agent-ready"), secret, http.StatusOK, "already running"},
	} {
		code, body := post(t, h, tc.body, tc.token)
		if code != tc.code || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %q, want %d containing %q", tc.name, code, body, tc.code, tc.want)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := s.Start(ctx, 1)
	deadline := time.Now().Add(5 * time.Second)
	for len(r.ran()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.ran(); len(got) != 1 || got[0] != "P-1" {
		t.Errorf("ran %v, want [P-1]", got)
	}
	// Once finished, the same ticket can be queued again.
	for {
		s.mu.Lock()
		busy := s.active["P-1"]
		s.mu.Unlock()
		if !busy || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := post(t, h, issueEvent("P-1", "agent-ready"), secret); code != http.StatusAccepted {
		t.Errorf("requeue after completion = %d", code)
	}
	cancel()
	<-done
}

func TestQueueFull(t *testing.T) {
	s := newServer(t, &fakeRunner{}, 1)
	h := s.Handler()
	if code, _ := post(t, h, issueEvent("P-1", "agent-ready"), secret); code != http.StatusAccepted {
		t.Fatalf("first = %d", code)
	}
	if code, _ := post(t, h, issueEvent("P-2", "agent-ready"), secret); code != http.StatusServiceUnavailable {
		t.Errorf("second = %d, want 503 with a full queue", code)
	}
}

func TestRunErrorsAreContained(t *testing.T) {
	r := &fakeRunner{err: errors.New("boom")}
	s := newServer(t, r, 1)
	s.active["P-9"] = true
	s.runOne(context.Background(), "P-9")
	if s.active["P-9"] {
		t.Error("a failed run stayed marked active")
	}
}

func TestHealthAndValidation(t *testing.T) {
	s := newServer(t, &fakeRunner{}, 1)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d", rec.Code)
	}
	if _, err := New(Options{Runner: &fakeRunner{}}); err == nil {
		t.Error("New accepted an empty secret")
	}
	if _, err := New(Options{Secret: "x"}); err == nil {
		t.Error("New accepted a nil runner")
	}
	big := strings.Repeat("x", maxBody+1)
	if code, _ := post(t, s.Handler(), big, secret); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body = %d", code)
	}
}
