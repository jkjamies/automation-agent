# internal

All non-entrypoint code. Two families:

- **Agents** (`agent/`) — the LLM-driven workflow agents and their shared `setup`
  utilities.
- **Tooling** (`config`, `ingest`, `githubapi`, `gitrepo`, `webhook`, `notify`,
  `scheduler`, `reconcile`) — deterministic, unit-testable, **agent-free**. These
  must not import `agent/...` (enforced by `ARCH/`).

## Flow

```mermaid
flowchart LR
    subgraph agents["internal/agent (LLM-driven)"]
        root --> summary
        root --> lintfixer
        summary --> setup
        lintfixer --> setup
    end
    subgraph tooling["internal/* (deterministic, agent-free)"]
        config
        ingest
        githubapi
        gitrepo
        webhook
        notify
        scheduler
        reconcile
    end
    agents -->|"may import"| tooling
    tooling -.->|"must NOT import (ARCH)"| agents
    setup -->|"only place allowed"| providers["provider SDKs: Ollama / Gemini / genai"]
```

This separation is what keeps the system testable to ≥80% coverage: the hard logic
lives in tooling and in agents' `<name>.go` files, both injectable and LLM-free.
