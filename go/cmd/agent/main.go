// Command agent is the automation-agent service entrypoint. It wires configuration,
// tooling, agents, the scheduler, and the webhook server together, then runs until
// interrupted. Composition only — logic lives in internal/.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/agent/covfixer"
	"github.com/jkjamies/automation-agent/internal/agent/fixflow"
	"github.com/jkjamies/automation-agent/internal/agent/lintfixer"
	"github.com/jkjamies/automation-agent/internal/agent/root"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
	"github.com/jkjamies/automation-agent/internal/agent/summary"
	"github.com/jkjamies/automation-agent/internal/config"
	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/ingest"
	"github.com/jkjamies/automation-agent/internal/notify"
	"github.com/jkjamies/automation-agent/internal/scheduler"
	"github.com/jkjamies/automation-agent/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Load .env if present (no-op when absent); real environment still wins.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		logger.Warn("could not load .env", "err", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	llm, err := setup.BuildLLM(sigCtx, cfg)
	if err != nil {
		return fmt.Errorf("build llm: %w", err)
	}
	codeLLM, err := setup.BuildCodeLLM(sigCtx, cfg)
	if err != nil {
		return fmt.Errorf("build code llm: %w", err)
	}
	gh := githubapi.New(cfg.GitHubToken)
	notifier := buildNotifier(logger, cfg)

	// One session service + park store, shared by both fix engines (namespaced by app
	// name). memory (default) keeps today's behavior; durable backends persist parked runs
	// across restarts.
	sessions, err := setup.NewSessionService(sigCtx, cfg)
	if err != nil {
		return fmt.Errorf("build session service: %w", err)
	}
	// Release a network-backed session service's client (e.g. firestore) on shutdown.
	if closer, ok := sessions.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	parkStore, err := setup.NewParkStore(sigCtx, cfg)
	if err != nil {
		return fmt.Errorf("build park store: %w", err)
	}
	// Release a network-backed store's client (e.g. firestore) on shutdown.
	if closer, ok := parkStore.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	// Summary workflow (needs repos + a notifier). Daily and weekly are distinct agents
	// so the weekly cron posts a real 7-day digest, not a copy of the daily one.
	summaryDaily := buildSummaryAgent(logger, cfg, llm, gh, notifier, 24*time.Hour, "Daily commit digest")
	summaryWeekly := buildSummaryAgent(logger, cfg, llm, gh, notifier, 7*24*time.Hour, "Weekly commit digest")

	// Fix engines (event-driven; work without a notifier — they just won't post results).
	fixDeps := fixflow.Deps{
		LLM: llm, CodeLLM: codeLLM, GH: gh, Notify: notifier, Token: cfg.GitHubToken,
		MaxIter: cfg.MaxIterations, CITimeout: cfg.CITimeout, Repos: cfg.Repos, Log: logger,
		PRLabel:        cfg.AgentPRLabel,
		SessionService: sessions, ParkStore: parkStore,
	}
	lintEngine := lintfixer.NewEngine(fixDeps)
	covEngine := covfixer.NewEngine(fixDeps)
	engines := []*fixflow.Engine{lintEngine, covEngine}

	dispatcher, err := root.BuildRootDispatcher(root.Deps{
		SummaryDaily:    summaryDaily,
		SummaryWeekly:   summaryWeekly,
		LintKickoff:     payloadHandler(lintEngine.Kickoff),
		CoverageKickoff: payloadHandler(covEngine.Kickoff),
		CIResume:        ciResumeHandler(engines),
		Log:             logger,
	})
	if err != nil {
		return fmt.Errorf("build dispatcher: %w", err)
	}

	// Scheduler: cron → dispatch (cron runs each job in its own goroutine).
	sched := scheduler.New(func(e ingest.Envelope) {
		if err := dispatcher.Dispatch(context.Background(), e); err != nil {
			logger.Error("scheduled dispatch failed", "kind", e.Kind, "err", err)
		}
	})
	if err := sched.Add(cfg.CronDaily, ingest.KindCronDaily); err != nil {
		return fmt.Errorf("schedule daily: %w", err)
	}
	if err := sched.Add(cfg.CronWeekly, ingest.KindCronWeekly); err != nil {
		return fmt.Errorf("schedule weekly: %w", err)
	}

	// Webhooks enqueue asynchronously and return fast. Dispatches run on a bounded pool
	// and are tracked so a SIGTERM drains in-flight work instead of dropping it. (With a
	// durable SESSION_BACKEND parked runs survive a restart; the default memory backend
	// does not.)
	var dispatchWG sync.WaitGroup
	dispatchSem := make(chan struct{}, maxConcurrentDispatch)
	if cfg.GitHubWebhookSecret == "" {
		logger.Warn("GITHUB_WEBHOOK_SECRET is unset — webhook signatures are NOT verified; the /webhooks/github route accepts unauthenticated requests (dev only)")
	}
	srv := webhook.New(func(_ context.Context, e ingest.Envelope) error {
		dispatchSem <- struct{}{} // bound concurrency (backpressure under burst)
		dispatchWG.Add(1)
		go func() {
			defer dispatchWG.Done()
			defer func() { <-dispatchSem }()
			if err := dispatcher.Dispatch(context.Background(), e); err != nil {
				logger.Error("webhook dispatch failed", "kind", e.Kind, "err", err)
			}
		}()
		return nil
	}, webhook.WithGitHubSecret(cfg.GitHubWebhookSecret),
		webhook.WithInternalToken(cfg.InternalToken),
		webhook.WithSweep(func(ctx context.Context) error {
			// Sweep every engine even if one fails, so a single engine's error does not
			// strand the others' timed-out runs for this pass (mirrors ciResumeHandler).
			// The joined error still 500s the handler so Cloud Scheduler retries.
			var errs []error
			for _, e := range engines {
				if err := e.SweepTimeouts(ctx); err != nil {
					errs = append(errs, err)
					logger.Error("engine sweep failed", "workflow", e.Name(), "err", err)
				}
			}
			return errors.Join(errs...)
		}))

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	sched.Start()

	go func() {
		logger.Info("automation-agent listening",
			"port", cfg.Port,
			"llm_provider", cfg.LLMProvider,
			"repos", len(cfg.Repos),
			"notify", cfg.NotifyProvider,
			"summary_enabled", summaryDaily != nil,
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "err", err)
		}
	}()

	<-sigCtx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	}
	drain(logger, sched.Stop(), &dispatchWG, drainTimeout)
	return nil
}

// maxConcurrentDispatch bounds in-flight webhook dispatches; drainTimeout caps how long
// shutdown waits for in-flight dispatches and scheduled jobs to finish.
const (
	maxConcurrentDispatch = 32
	drainTimeout          = 15 * time.Second
)

// drain waits for in-flight webhook dispatches (wg) and running scheduled jobs (stopCtx,
// from cron.Stop) to finish, bounded by timeout, so a clean SIGTERM completes work in
// flight rather than abandoning it.
func drain(logger *slog.Logger, stopCtx context.Context, wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		<-stopCtx.Done()
		close(done)
	}()
	select {
	case <-done:
		logger.Info("drained in-flight work")
	case <-time.After(timeout):
		logger.Warn("drain timed out; exiting with work still in flight")
	}
}

// buildNotifier returns a Notifier, or nil (with a warning) if not configured.
func buildNotifier(logger *slog.Logger, cfg config.Config) notify.Notifier {
	n, err := notify.New(string(cfg.NotifyProvider), cfg.SlackWebhookURL, cfg.TeamsWebhookURL)
	if err != nil {
		logger.Warn("notifier not configured; summary disabled and lint-fixer won't post", "err", err)
		return nil
	}
	return n
}

// buildSummaryAgent returns a summary workflow agent for the given commit window and
// notification title, or nil if it can't be fully configured (no repos or no notifier).
func buildSummaryAgent(logger *slog.Logger, cfg config.Config, llm model.LLM, gh summary.CommitLister, notifier notify.Notifier, window time.Duration, title string) agent.Agent {
	if len(cfg.Repos) == 0 {
		logger.Warn("no REPOS configured; summary workflow disabled")
		return nil
	}
	if notifier == nil {
		return nil // buildNotifier already warned
	}
	a, err := summary.BuildSummaryAgent(summary.Deps{LLM: llm, GH: gh, Notify: notifier, Repos: cfg.Repos, Window: window, Title: title})
	if err != nil {
		logger.Warn("summary workflow disabled", "err", err)
		return nil
	}
	return a
}

// payloadHandler adapts a raw-payload kickoff/resume func to a root.Handler.
func payloadHandler(f func(ctx context.Context, raw []byte) error) root.Handler {
	return func(ctx context.Context, e ingest.Envelope) error { return f(ctx, e.Payload) }
}

// ciResumeHandler hands a check_run event to every engine; each no-ops unless its
// check name matches.
func ciResumeHandler(engines []*fixflow.Engine) root.Handler {
	return func(ctx context.Context, e ingest.Envelope) error {
		var errs []error
		for _, eng := range engines {
			if err := eng.Resume(ctx, e.Payload); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}
