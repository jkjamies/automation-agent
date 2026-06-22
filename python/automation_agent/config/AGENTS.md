# automation_agent/config

Single source of truth for runtime configuration. Loaded once from the environment
(`config.load`) and passed down; **no other package reads `os.environ`**.

## Flow

```mermaid
flowchart TD
    A[caller / main] -->|os.environ.get| B["load()"]
    B -->|"lookup func"| C["load_from(get lookup)"]
    C --> D["get_or(get, key, default)"]
    D -->|"env set & non-empty"| E[use env value]
    D -->|missing or empty| F[use default]
    C --> G["split_list(REPOS)"]
    G -->|"comma split + trim"| H["list[str] repos"]
    C --> I["int(MAX_ITERATIONS)"]
    I -->|parse error| K["raise ValueError"]
    C --> L["c.validate()"]
    L --> M{llm_provider in ollama|gemini?}
    M -->|no| K
    L --> N{notify_provider in slack|teams?}
    N -->|no| K
    L --> O{max_iterations >= 1?}
    O -->|no| K
    M -->|yes| P
    N -->|yes| P
    O -->|yes| P[populated Config]
    P --> Q["return Config"]
```

- `config.py` — `load_from(lookup)` keeps loading testable without touching the real
  environment.
- `validate()` enforces invariants defaults can't (provider enums, `max_iterations >= 1`).
- See `.agents/standards/architecture-design.md` §12 and `.env.example` for the full variable list.
