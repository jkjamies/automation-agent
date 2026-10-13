// Command ticketflow takes Jira tickets to pull requests for human review.
//
//	ticketflow run [-dry-run] [-keep] PROJ-123   one ticket, in the foreground
//	ticketflow serve                            Jira webhook receiver + worker pool
//
// Configuration is environment-only; see .env.example.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"

	"ticketflow/internal/config"
	"ticketflow/internal/gate"
	"ticketflow/internal/github"
	"ticketflow/internal/harness"
	"ticketflow/internal/jira"
	"ticketflow/internal/pipeline"
	"ticketflow/internal/server"
	"ticketflow/internal/workspace"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(ctx, os.Args[1:], log); err != nil {
		fmt.Fprintln(os.Stderr, "ticketflow:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  ticketflow run [-dry-run] [-keep] ISSUE-KEY
  ticketflow serve`

func run(ctx context.Context, args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if err := cfg.RequireRun(); err != nil {
		return err
	}
	p, err := buildPipeline(ctx, cfg, log)
	if err != nil {
		return err
	}
	switch args[0] {
	case "run":
		return runOne(ctx, p, args[1:])
	case "serve":
		return serve(ctx, cfg, p, log)
	default:
		return errors.New(usage)
	}
}

func runOne(ctx context.Context, p *pipeline.Pipeline, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "do everything except push, open the PR, and write to Jira")
	keep := fs.Bool("keep", false, "keep the checkout for inspection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(usage)
	}
	out, err := p.Run(ctx, fs.Arg(0), pipeline.RunOptions{DryRun: *dry, KeepWorkdir: *keep})
	if out.Status == pipeline.StatusDryRun {
		fmt.Printf("── PR description ──\n%s\n── patch ──\n%s\n", out.PRBody, out.Patch)
		out.PRBody, out.Patch = "", ""
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(out); encErr != nil {
		return errors.Join(err, encErr)
	}
	return err
}

func serve(ctx context.Context, cfg config.Config, p *pipeline.Pipeline, log *slog.Logger) error {
	srv, err := server.New(server.Options{Secret: cfg.JiraWebhookSecret, TriggerLabel: cfg.TriggerLabel, Runner: p, Log: log})
	if err != nil {
		return err
	}
	done := srv.Start(ctx, cfg.Workers)
	hs := &http.Server{Addr: cfg.Addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hs.Shutdown(shutdown); err != nil {
			log.Warn("shutdown", "err", err)
		}
	}()
	log.Info("listening", "addr", cfg.Addr, "workers", cfg.Workers)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

func buildPipeline(ctx context.Context, cfg config.Config, log *slog.Logger) (*pipeline.Pipeline, error) {
	jc, err := jira.NewClient(cfg.JiraBaseURL, cfg.JiraEmail, cfg.JiraToken, nil)
	if err != nil {
		return nil, err
	}
	models := map[string]model.LLM{}
	vertex := func(name string) (model.LLM, error) {
		if m, ok := models[name]; ok {
			return m, nil
		}
		m, err := gemini.NewModel(ctx, name, &genai.ClientConfig{
			Backend:  genai.BackendVertexAI,
			Project:  cfg.GCPProject,
			Location: cfg.GCPLocation,
		})
		if err != nil {
			return nil, fmt.Errorf("vertex model %s: %w", name, err)
		}
		models[name] = m
		return m, nil
	}
	planner, err := vertex(cfg.PlannerModel)
	if err != nil {
		return nil, err
	}
	reviewer, err := vertex(cfg.ReviewerModel)
	if err != nil {
		return nil, err
	}
	var h harness.Harness
	switch cfg.Harness {
	case "cli":
		h = &harness.CLI{Argv: cfg.HarnessCommand, Env: cfg.HarnessEnv, Timeout: cfg.HarnessTimeout}
	default:
		coder, err := vertex(cfg.CoderModel)
		if err != nil {
			return nil, err
		}
		h = &harness.ADK{Model: coder, CheckTimeout: cfg.CheckTimeout, Log: log}
	}
	return pipeline.New(pipeline.Deps{
		Tracker:  jc,
		Host:     github.NewClient(cfg.GitHubAPI, cfg.GitHubToken, nil),
		Harness:  h,
		Planner:  planner,
		Reviewer: reviewer,
		Log:      log,
		Settings: pipeline.Settings{
			TriggerLabel:     cfg.TriggerLabel,
			ClaimLabel:       cfg.ClaimLabel,
			NeedsHumanLabel:  cfg.NeedsHumanLabel,
			ReviewTransition: cfg.ReviewTransition,
			ProjectRepos:     cfg.ProjectRepos,
			RepoAllowed:      cfg.RepoAllowed,
			BaseBranch:       cfg.BaseBranch,
			BranchPrefix:     cfg.BranchPrefix,
			PRLabel:          cfg.PRLabel,
			DraftPRs:         cfg.DraftPRs,
			Author:           workspace.Author{Name: cfg.BotName, Email: cfg.BotEmail},
			MaxAttempts:      cfg.MaxAttempts,
			MaxRevisions:     cfg.MaxRevisions,
			Limits:           gate.DiffLimits{MaxFiles: cfg.MaxFiles, MaxChangedLines: cfg.MaxChangedLines},
			Readiness:        gate.DefaultReadinessRules,
			CheckTimeout:     cfg.CheckTimeout,
			WorkRoot:         cfg.WorkRoot,
			CloneURL:         func(repo string) string { return cfg.GitHubHost + "/" + repo + ".git" },
			GitToken:         cfg.GitHubToken,
		},
	})
}
