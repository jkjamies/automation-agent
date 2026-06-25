# automation_agent

All non-entrypoint code. Two families:

- **Agents** (`agent/`) — the LLM-driven workflow agents and their shared `setup`
  utilities.
- **Tooling** (`config`, `ingest`, `githubapi`, `gitrepo`, `webhook`, `notify`) —
  deterministic, unit-testable, **agent-free**. These
  must not import `agent/...` (enforced by `arch/`).

## Flow

```mermaid
flowchart LR
    subgraph agents["automation_agent/agent (LLM-driven)"]
        root --> summary
        root --> lintfixer
        root --> covfixer
        lintfixer --> fixflow
        covfixer --> fixflow
        summary --> setup
        fixflow --> setup
    end
    subgraph tooling["automation_agent/* (deterministic, agent-free)"]
        config
        ingest
        githubapi
        gitrepo
        webhook
        notify
    end
    agents -->|"may import"| tooling
    tooling -.->|"must NOT import (arch)"| agents
    setup -->|"only place allowed"| providers["provider SDKs: LiteLlm (Ollama) / Gemini / genai"]
```

This separation is what keeps the system testable to >=80% coverage: the hard logic
lives in tooling and in agents' logic files, both injectable and LLM-free.
