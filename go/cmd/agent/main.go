// Command agent is the automation-agent service entrypoint. It wires configuration,
// tooling, agents, and the webhook server together, then runs until interrupted.
// Composition only — logic lives in internal/.
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
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"

	"automation-agent/internal/agent/covfixer"
	"automation-agent/internal/agent/fixflow"
	"automation-agent/internal/agent/lintfixer"
	"automation-agent/internal/agent/reviewer"
	"automation-agent/internal/agent/root"
	"automation-agent/internal/agent/setup"
	"automation-agent/internal/agent/summary"
	"automation-agent/internal/auth"
	"automation-agent/internal/config"
	"automation-agent/internal/githubapi"
	"automation-agent/internal/ingest"
	"automation-agent/internal/notify"
	"automation-agent/internal/tasks"
	"automation-agent/internal/webhook"
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
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	provider, err := buildTokenProvider(logger, cfg)
	if err != nil {
		return fmt.Errorf("build token provider: %w", err)
	}
	// Resolve the login this deployment authors comments as ("<slug>[bot]" in App mode, the user
	// in PAT mode) so the reviewer's marker-comment upsert edits only its own comments. Best
	// effort: a lookup failure must not block startup, so warn and let the client fall back to
	// author-type matching.
	var authoredLogin string
	if ir, ok := provider.(auth.IdentityResolver); ok {
		// Bound the lookup so a slow GitHub API can't hang startup; cancel right after (not via
		// defer — run() lives for the whole server lifetime).
		lookupCtx, cancel := context.WithTimeout(sigCtx, 10*time.Second)
		login, lookupErr := ir.AuthoredLogin(lookupCtx)
		cancel()
		if lookupErr != nil {
			logger.Warn("could not resolve GitHub comment-author identity; marker upsert falls back to author-type matching", "err", lookupErr)
		} else {
			authoredLogin = login
		}
	}
	gh := githubapi.New(provider, githubapi.WithAuthoredLogin(authoredLogin))
	// SSH only authenticates the git transport (clone/push). The GitHub REST API — opening
	// and labeling PRs, reading the CI check — still needs a token (or `gh` login). Warn
	// rather than fail so read-only/dry-run flows still work, but PR operations will not.
	// In App mode the provider always yields a token, so this only applies to PAT mode.
	if !cfg.AppMode() && cfg.GitTransport == "ssh" && cfg.GitHubToken == "" {
		logger.Warn("GIT_TRANSPORT=ssh but no GitHub token found (GITHUB_TOKEN/GH_TOKEN/`gh auth token`); git clone+push will use ssh, but PR operations against the REST API will fail — run `gh auth login` or set a token")
	}
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

	// Summary workflow (needs repos + a notifier). The daily Cloud Scheduler trigger fires it.
	summaryDaily := buildSummaryAgent(logger, cfg, llm, gh, notifier, 24*time.Hour, "Daily commit digest")
	// /internal/cron/daily is the only daily-digest trigger, and it 404s when INTERNAL_TOKEN
	// is unset. Warn rather than fail silently so a built-but-unreachable digest is visible.
	if summaryDaily != nil && cfg.InternalToken == "" {
		logger.Warn("daily summary built but INTERNAL_TOKEN is unset; /internal/cron/daily is disabled (404), so the digest cannot be triggered")
	}

	// Fix engines (event-driven; work without a notifier — they just won't post results).
	fixDeps := fixflow.Deps{
		LLM: llm, CodeLLM: codeLLM, GH: gh, Notify: notifier, Provider: provider,
		MaxIter: cfg.MaxIterations, CITimeout: cfg.CITimeout, Repos: cfg.Repos, Log: logger,
		PRLabel:        cfg.AgentPRLabel,
		SessionService: sessions, ParkStore: parkStore,
		GitTransport: cfg.GitTransport, SSHKey: cfg.GitSSHKey,
	}
	lintEngine := lintfixer.NewEngine(fixDeps)
	covEngine := covfixer.NewEngine(fixDeps)
	engines := []*fixflow.Engine{lintEngine, covEngine}

	// PR code-review agent (reacts to pull_request → KindReview). Always registered; the
	// engine no-ops unless REVIEW_ENABLED is set, so REVIEW_ENABLED is the kill switch.
	reviewEngine := reviewer.NewEngine(reviewer.Deps{
		Enabled:           cfg.ReviewEnabled,
		GH:                gh,
		BaseLLM:           llm,
		CodeLLM:           codeLLM,
		MinConfidence:     cfg.ReviewMinConfidence,
		SkipDrafts:        cfg.ReviewSkipDrafts,
		ExcludeGlobs:      cfg.ReviewExcludeGlobs,
		MaxFiles:          cfg.ReviewMaxFiles,
		MaxDiffBytes:      cfg.ReviewMaxDiffBytes,
		StandardsEnabled:  cfg.ReviewStandards,
		StandardsGlobs:    cfg.ReviewStandardsGlobs,
		StandardsMaxBytes: cfg.ReviewStandardsMaxBytes,
		UncitedDrop:       cfg.ReviewUncitedMode == "drop",
		Log:               logger,
	})

	dispatcher, err := root.BuildRootDispatcher(root.Deps{
		SummaryDaily:    summaryDaily,
		LintKickoff:     payloadHandler(lintEngine.Kickoff),
		CoverageKickoff: payloadHandler(covEngine.Kickoff),
		CIResume:        ciResumeHandler(engines),
		ReviewKickoff:   payloadHandler(reviewEngine.Kickoff),
		Log:             logger,
	})
	if err != nil {
		return fmt.Errorf("build dispatcher: %w", err)
	}

	// Webhooks enqueue asynchronously and return fast. The transport runs the dispatch:
	// in-process (default) on a bounded goroutine pool drained on SIGTERM, or — in
	// production — via Cloud Tasks, which delivers each envelope to /internal/dispatch so
	// the compute runs in-request (CPU stays allocated) with durable retry. See
	// specs/20260626-workflow-execution-transport.md.
	transport, err := buildTransport(sigCtx, logger, cfg, dispatcher.Dispatch)
	if err != nil {
		return fmt.Errorf("build task transport: %w", err)
	}

	if cfg.GitHubWebhookSecret == "" {
		logger.Warn("GITHUB_WEBHOOK_SECRET is unset — webhook signatures are NOT verified; the /webhooks/github route accepts unauthenticated requests (dev only)")
	}
	srv := webhook.New(
		func(ctx context.Context, e ingest.Envelope) error {
			// Review envelopes carry coalescing hints (debounce + per-PR dedup name) so rapid
			// pushes collapse to one task on the latest SHA; other kinds enqueue immediately.
			return transport.Enqueue(ctx, e, reviewer.EnqueueOptions(e, cfg.ReviewDebounce)...)
		},
		webhook.WithGitHubSecret(cfg.GitHubWebhookSecret),
		webhook.WithInternalToken(cfg.InternalToken),
		// /internal/dispatch executes a queued envelope in-request (the Cloud Tasks worker).
		webhook.WithDispatch(dispatcher.Dispatch),
		webhook.WithLogger(logger),
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
	// Close the transport after the server stops accepting: the in-process backend drains
	// in-flight dispatches (bounded), the Cloud Tasks backend closes its client. Done before
	// the deferred session/park-store closers so any draining dispatch still has its stores.
	if err := transport.Close(); err != nil {
		logger.Error("transport close", "err", err)
	}
	return nil
}

// buildTransport selects the webhook execution transport: Cloud Tasks in production
// (durable, in-request, rate-limited by the queue) or the in-process goroutine pool for
// local dev (the default). See specs/20260626-workflow-execution-transport.md.
func buildTransport(ctx context.Context, logger *slog.Logger, cfg config.Config, dispatch tasks.DispatchFunc) (tasks.Transport, error) {
	if cfg.TasksBackend == config.TasksCloudTasks {
		t, err := tasks.NewCloudTasks(ctx, cfg.TasksProject, cfg.TasksLocation, cfg.TasksQueue, cfg.DispatchURL, cfg.InternalToken, cfg.TasksDispatchDeadline)
		if err != nil {
			return nil, err
		}
		logger.Info("execution transport: cloud tasks",
			"project", cfg.TasksProject, "location", cfg.TasksLocation, "queue", cfg.TasksQueue, "dispatch_url", cfg.DispatchURL)
		return t, nil
	}
	logger.Info("execution transport: in-process (local/default)")
	return tasks.NewInProcess(dispatch, logger, tasks.DefaultMaxConcurrent), nil
}

// buildTokenProvider selects the GitHub auth path: App installation tokens in
// production (when the GITHUB_APP_* vars are set — auto-minted, repo-scoped,
// short-lived), else the static PAT fallback for local dev. The provider is shared
// by the REST client and the git transport.
func buildTokenProvider(logger *slog.Logger, cfg config.Config) (auth.TokenProvider, error) {
	if cfg.AppMode() {
		p, err := auth.NewAppProvider(nil, cfg.GitHubApp.AppID, cfg.GitHubApp.InstallationID, cfg.GitHubApp.PrivateKeyPEM)
		if err != nil {
			return nil, err
		}
		logger.Info("github auth: app mode", "app_id", cfg.GitHubApp.AppID, "installation_id", cfg.GitHubApp.InstallationID)
		return p, nil
	}
	logger.Info("github auth: pat mode (local-dev fallback)")
	return auth.NewStaticProvider(cfg.GitHubToken), nil
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
