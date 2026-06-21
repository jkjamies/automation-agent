# cmd/playground

A local-only entrypoint that launches ADK's embedded web UI (or a one-shot CLI) to
interact with the configured model. **Development only** — a separate entrypoint from
`cmd/agent`, so it is never in a production artifact, yet still imported by the test
suite / lint pass (preferred over conditional skips, which would hide breakage).

## Flow

```mermaid
flowchart TD
    Make["make playground"] --> Run["python -m cmd.playground web"]
    Run --> Env["load .env (auto-loaded)"]
    Env --> Cfg["config.load()"]
    Cfg --> LLM["setup.build_llm(cfg) -> BaseLlm (Ollama via LiteLlm / Gemini)"]
    LLM --> Agent["LlmAgent(playground chat agent)"]
    Agent --> Loader["single-agent loader (chat)"]
    Loader --> L["adk web / adk run launcher"]
    L --> U{"launcher mode"}
    U -->|console| CLI["interactive CLI chat"]
    U -->|web| W["web UI"]
    W -->|"api + webui"| UI["REST API + embedded web UI @ :8080"]
    W -->|a2a| A2A["A2A protocol endpoint"]
```

Modes: the web UI serves the REST API + embedded UI, so `make playground` runs the
web launcher (per the ADK docs). `console` gives an interactive CLI. `.env` is auto-loaded.

To drive the real workflows interactively, swap the chat agent for the
`summary`/`lintfixer` agents in the entrypoint. Not part of the prod deploy (which runs
only `cmd/agent`).
