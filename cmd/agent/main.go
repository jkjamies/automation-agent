// Command agent is the automation-agent service entrypoint. It wires configuration,
// tooling, agents, the scheduler, and the webhook server together, then runs until
// interrupted. Composition only — all logic lives in internal/.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"

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
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	llm, err := setup.BuildLLM(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build llm: %w", err)
	}
	gh := githubapi.New(cfg.GitHubToken)

	// The summary workflow needs both repos and a working notifier; if either is
	// missing we start webhooks-only rather than failing to boot.
	summaryAgent := buildSummaryAgent(logger, cfg, llm, gh)

	dispatcher, err := root.BuildRootDispatcher(root.Deps{SummaryAgent: summaryAgent, Log: logger})
	if err != nil {
		return fmt.Errorf("build dispatcher: %w", err)
	}

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

	sched.Start()
	defer sched.Stop()

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

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// buildSummaryAgent returns the summary workflow agent, or nil (with a warning) if
// it can't be fully configured.
func buildSummaryAgent(logger *slog.Logger, cfg config.Config, llm model.LLM, gh summary.CommitLister) agent.Agent {
	if len(cfg.Repos) == 0 {
		logger.Warn("no REPOS configured; summary workflow disabled")
		return nil
	}
	notifier, err := notify.New(string(cfg.NotifyProvider), cfg.SlackWebhookURL, cfg.TeamsWebhookURL)
	if err != nil {
		logger.Warn("notifier not configured; summary workflow disabled", "err", err)
		return nil
	}
	a, err := summary.BuildSummaryAgent(summary.Deps{
		LLM:    llm,
		GH:     gh,
		Notify: notifier,
		Repos:  cfg.Repos,
	})
	if err != nil {
		logger.Warn("summary workflow disabled", "err", err)
		return nil
	}
	return a
}
