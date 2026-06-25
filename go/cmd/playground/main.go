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
	"log"
	"os"

	"github.com/joho/godotenv"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/config"
)

func main() {
	ctx := context.Background()
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	llm, err := setup.BuildLLM(ctx, cfg)
	if err != nil {
		log.Fatalf("build llm: %v", err)
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
		log.Fatalf("build agent: %v", err)
	}

	l := full.NewLauncher()
	if err := l.Execute(ctx, &launcher.Config{AgentLoader: agent.NewSingleLoader(chat)}, os.Args[1:]); err != nil {
		log.Fatalf("playground failed: %v\n\n%s", err, l.CommandLineSyntax())
	}
}
