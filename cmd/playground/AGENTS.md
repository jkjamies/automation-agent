# cmd/playground

A local-only entrypoint that launches ADK's embedded web UI (or a one-shot CLI) to
interact with the configured model. **Development only** — a separate binary from
`cmd/agent`, so it is never in a production artifact, yet still compiled by
`go build ./...`/`make ci` (preferred over a build tag, which would hide breakage).

## Flow

```mermaid
flowchart TD
    Make["make playground"] --> Run["go run ./cmd/playground web api webui"]
    Run --> Env["godotenv.Load() (.env auto-loaded)"]
    Env --> Cfg["config.Load()"]
    Cfg --> LLM["setup.BuildLLM(ctx, cfg) -> model.LLM (Ollama/Gemini)"]
    LLM --> Agent["llmagent.New(playground chat agent)"]
    Agent --> Loader["agent.NewSingleLoader(chat)"]
    Loader --> L["full.NewLauncher().Execute(ctx, launcher.Config, os.Args)"]
    L --> U{"universal launcher keyword"}
    U -->|console| CLI["interactive CLI chat"]
    U -->|web| W["web (accepts multiple sublaunchers)"]
    W -->|"api webui"| UI["REST API + embedded web UI @ :8080"]
    W -->|a2a| A2A["A2A protocol endpoint"]
```

Subcommands (from `full.NewLauncher` = `universal(console, web(webui, api, a2a, …))`):
the web UI needs **both** `api` and `webui`, so `make playground` runs `web api webui`
(per the ADK docs). `console` gives an interactive CLI. `.env` is auto-loaded.

To drive the real workflows interactively, swap the chat agent for the
`summary`/`lintfixer` agents in `main.go`. Not part of the prod deploy (which builds
only `./cmd/agent`).
