// Package server receives Jira webhooks and runs the pipeline for tickets handed to the
// agent. It authenticates deliveries, decides whether one is a trigger, deduplicates
// tickets already in flight, and runs them on a bounded worker pool.
//
// Runs happen in-process after the 202. That suits a single always-on instance; on a
// scale-to-zero platform the queue belongs in infrastructure instead (see docs/design.md).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"ticketflow/internal/jira"
	"ticketflow/internal/pipeline"
)

// Runner runs one ticket.
type Runner interface {
	Run(ctx context.Context, key string, opts pipeline.RunOptions) (pipeline.Outcome, error)
}

// Options configure a Server.
type Options struct {
	Secret       string // webhook secret; required
	TriggerLabel string
	Runner       Runner
	QueueSize    int           // pending runs beyond which deliveries get 503; default 64
	RunTimeout   time.Duration // per run; default 2h
	Log          *slog.Logger
}

// Server is the webhook front door.
type Server struct {
	o      Options
	queue  chan string
	mu     sync.Mutex
	active map[string]bool
}

// New builds a server. Call Start to begin processing and Handler to serve.
func New(o Options) (*Server, error) {
	if o.Secret == "" {
		return nil, errors.New("server: a webhook secret is required")
	}
	if o.Runner == nil {
		return nil, errors.New("server: a runner is required")
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 64
	}
	if o.RunTimeout <= 0 {
		o.RunTimeout = 2 * time.Hour
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Server{o: o, queue: make(chan string, o.QueueSize), active: map[string]bool{}}, nil
}

// maxBody caps a webhook delivery; Jira issue payloads are far smaller.
const maxBody = 1 << 20

// Handler serves POST /webhooks/jira and GET /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("POST /webhooks/jira", s.jiraWebhook)
	return mux
}

func (s *Server) jiraWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if !jira.Authentic(s.o.Secret, r.Header, body) {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	ev, err := jira.ParseEvent(body)
	switch {
	case errors.Is(err, jira.ErrNotIssueEvent):
		reply(w, http.StatusOK, "ignored", "not an issue event")
		return
	case err != nil:
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !slices.Contains(ev.Labels, s.o.TriggerLabel) {
		reply(w, http.StatusOK, "ignored", "no "+s.o.TriggerLabel+" label")
		return
	}
	s.mu.Lock()
	if s.active[ev.IssueKey] {
		s.mu.Unlock()
		reply(w, http.StatusOK, "ignored", "already running")
		return
	}
	select {
	case s.queue <- ev.IssueKey:
		s.active[ev.IssueKey] = true
		s.mu.Unlock()
	default:
		s.mu.Unlock()
		reply(w, http.StatusServiceUnavailable, "busy", "queue full; retry later")
		return
	}
	s.o.Log.InfoContext(r.Context(), "ticket queued", "ticket", ev.IssueKey, "event", ev.Name)
	reply(w, http.StatusAccepted, "queued", ev.IssueKey)
}

func reply(w http.ResponseWriter, code int, status, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "detail": detail}) // the status line is already sent
}

// Start runs workers until ctx is cancelled; it returns immediately. Done is closed once
// every worker has exited.
func (s *Server) Start(ctx context.Context, workers int) (done <-chan struct{}) {
	if workers <= 0 {
		workers = 1
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case key := <-s.queue:
					s.runOne(ctx, key)
				}
			}
		}()
	}
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	return ch
}

func (s *Server) runOne(ctx context.Context, key string) {
	defer func() {
		s.mu.Lock()
		delete(s.active, key)
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, s.o.RunTimeout)
	defer cancel()
	out, err := s.o.Runner.Run(ctx, key, pipeline.RunOptions{})
	if err != nil {
		s.o.Log.ErrorContext(ctx, "run failed", "ticket", key, "status", out.Status, "err", err)
		return
	}
	s.o.Log.InfoContext(ctx, "run complete", "ticket", key, "status", out.Status, "pr", out.PRURL)
}
