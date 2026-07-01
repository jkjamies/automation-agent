// Command playground launches a local ADK web UI (or one-shot CLI) to interact with
// the configured model, for local development only.
//
// It is a SEPARATE binary from cmd/agent: production builds target ./cmd/agent, so
// the playground is never part of a deployed artifact — while `go build ./...` and
// `make ci` still compile it, so breakage is caught. This is preferred over a build
// tag, which would hide the file from CI.
//
// The launcher (full.NewLauncher) exposes these subcommands; the web UI needs both
// the api backend and the webui front-end, so they are given together:
//   - web api webui        REST API + embedded web UI (default :8080)
//   - console              interactive CLI chat
//   - web a2a              expose the agent over the A2A protocol
//
// This binary loads .env itself (godotenv), so unlike the ADK quickstart you do not
// need to `source .env` first.
//
// Usage:
//
//	make playground                            # web UI at http://localhost:8080
//	go run ./cmd/playground web api webui      # same
//	go run ./cmd/playground console            # interactive CLI
//	go run ./cmd/playground --help             # full launcher syntax
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/config"
	"automation-agent/internal/obs"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run wires the playground and blocks on the launcher. It returns errors rather than
// calling log.Fatalf directly so that deferred cleanup — notably the tracing shutdown flush
// — runs on every exit path (os.Exit, which log.Fatal calls, skips defers).
func run() error {
	ctx := context.Background()
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Default the playground to the console exporter so a developer sees the span tree on
	// stdout with no backend to stand up — but respect an explicit OTEL_TRACES_EXPORTER.
	exporter := cfg.OTELTracesExporter
	if _, set := os.LookupEnv("OTEL_TRACES_EXPORTER"); !set {
		exporter = config.OTELExporterConsole
	}
	shutdownTracing, err := obs.Init(ctx, obs.Config{
		Exporter:     exporter,
		ServiceName:  cfg.OTELServiceName,
		OTLPEndpoint: cfg.OTELExporterOTLPEndpoint,
		OTLPHeaders:  cfg.OTELExporterOTLPHeaders,
		Sampler:      cfg.OTELTracesSampler,
	})
	if err != nil {
		return fmt.Errorf("init observability: %w", err)
	}
	// Flushes buffered spans on the way out — reached on the normal return and every error
	// return below, which is why those paths return rather than log.Fatalf.
	defer func() { _ = shutdownTracing(ctx) }()

	llm, err := setup.BuildLLM(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build llm: %w", err)
	}

	// A simple chat agent over the configured model. Swap in summary/lintfixer
	// agents here to drive the real workflows interactively.
	chat, err := llmagent.New(llmagent.Config{
		Name:        "automation_agent_playground",
		Description: "Local playground for poking the configured model.",
		Model:       llm,
		Instruction: "You are the automation-agent local playground, backed by the configured model. Help the developer test prompts. Be concise.",
	})
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}

	l := full.NewLauncher()
	if err := l.Execute(ctx, &launcher.Config{AgentLoader: agent.NewSingleLoader(chat)}, os.Args[1:]); err != nil {
		return fmt.Errorf("playground failed: %w\n\n%s", err, l.CommandLineSyntax())
	}
	return nil
}
