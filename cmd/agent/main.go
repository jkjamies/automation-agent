// Command agent is the automation-agent service entrypoint. It wires configuration,
// tooling, agents, the scheduler, the webhook server, and the reconcile loop
// together, then runs until interrupted. Composition only — logic lives in internal/.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/agent/lintfixer"
	"github.com/jkjamies/automation-agent/internal/agent/root"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
	"github.com/jkjamies/automation-agent/internal/agent/summary"
	"github.com/jkjamies/automation-agent/internal/config"
	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/ingest"
	"github.com/jkjamies/automation-agent/internal/notify"
	"github.com/jkjamies/automation-agent/internal/reconcile"
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
	gh := githubapi.New(cfg.GitHubToken)
	notifier := buildNotifier(logger, cfg)

	// Summary workflow (needs repos + a notifier).
	summaryAgent := buildSummaryAgent(logger, cfg, llm, gh, notifier)

	// Lint-fixer (event-driven; works without a notifier — it just won't post results).
	fixer := lintfixer.NewFixer(lintfixer.Deps{
		LLM: llm, GH: gh, Notify: notifier, Token: cfg.GitHubToken,
		Label: cfg.AgentPRLabel, CheckName: cfg.AgentCheckName, MaxIter: cfg.MaxIterations, Log: logger,
	})

	dispatcher, err := root.BuildRootDispatcher(root.Deps{
		SummaryAgent: summaryAgent,
		LintKickoff:  func(ctx context.Context, e ingest.Envelope) error { return fixer.Kickoff(ctx, e.Payload) },
		LintResume:   func(ctx context.Context, e ingest.Envelope) error { return fixer.Resume(ctx, e.Payload) },
		Log:          logger,
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

	// Webhooks enqueue asynchronously and return fast.
	srv := webhook.New(func(_ context.Context, e ingest.Envelope) error {
		go func() {
			if err := dispatcher.Dispatch(context.Background(), e); err != nil {
				logger.Error("webhook dispatch failed", "kind", e.Kind, "err", err)
			}
		}()
		return nil
	}, webhook.WithGitHubSecret(cfg.GitHubWebhookSecret))

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Reconcile loop: stateless recovery for missed CI webhooks + timeouts.
	reconciler := reconcile.New(gh, notifier, func(ctx context.Context, a reconcile.Action) error {
		owner, name, _ := strings.Cut(a.Repo, "/")
		return fixer.HandleResume(ctx, lintfixer.ResumeInput{
			Owner: owner, Repo: name, FullRepo: a.Repo,
			PRNumber: a.PR.Number, Branch: a.PR.Branch, HeadSHA: a.PR.HeadSHA,
			Conclusion: a.Check.Conclusion, OutputText: a.Check.OutputText,
		})
	}, reconcile.Config{
		Repos: cfg.Repos, Label: cfg.AgentPRLabel, CheckName: cfg.AgentCheckName, CITimeout: cfg.CITimeout,
	})

	sched.Start()
	defer sched.Stop()
	go runReconcileLoop(sigCtx, reconciler, cfg.ReconcileInterval, logger)

	go func() {
		logger.Info("automation-agent listening",
			"port", cfg.Port,
			"llm_provider", cfg.LLMProvider,
			"repos", len(cfg.Repos),
			"notify", cfg.NotifyProvider,
			"summary_enabled", summaryAgent != nil,
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "err", err)
		}
	}()

	<-sigCtx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
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

// buildSummaryAgent returns the summary workflow agent, or nil if it can't be fully
// configured (no repos or no notifier).
func buildSummaryAgent(logger *slog.Logger, cfg config.Config, llm model.LLM, gh summary.CommitLister, notifier notify.Notifier) agent.Agent {
	if len(cfg.Repos) == 0 {
		logger.Warn("no REPOS configured; summary workflow disabled")
		return nil
	}
	if notifier == nil {
		return nil // buildNotifier already warned
	}
	a, err := summary.BuildSummaryAgent(summary.Deps{LLM: llm, GH: gh, Notify: notifier, Repos: cfg.Repos})
	if err != nil {
		logger.Warn("summary workflow disabled", "err", err)
		return nil
	}
	return a
}

// runReconcileLoop scans on startup and on a ticker until the context is cancelled.
func runReconcileLoop(ctx context.Context, r *reconcile.Reconciler, interval time.Duration, logger *slog.Logger) {
	scan := func() {
		if _, err := r.Scan(context.Background()); err != nil {
			logger.Warn("reconcile scan", "err", err)
		}
	}
	scan()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			scan()
		}
	}
}
