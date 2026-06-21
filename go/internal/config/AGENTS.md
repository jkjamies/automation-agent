# internal/config

Single source of truth for runtime configuration. Loaded once from the environment
(`config.Load`) and passed down; **no other package reads `os.Getenv`**.

## Flow

```mermaid
flowchart TD
    A[caller / main] -->|os.LookupEnv| B["Load()"]
    B -->|"lookup func"| C["loadFrom(get lookup)"]
    C --> D["getOr(get, key, def)"]
    D -->|"env set & non-empty"| E[use env value]
    D -->|missing or empty| F[use default]
    C --> G["splitList(REPOS)"]
    G -->|"comma split + trim"| H["[]string Repos"]
    C --> I["strconv.Atoi MAX_ITERATIONS"]
    I -->|parse error| K["return Config{}, err"]
    C --> L["c.Validate()"]
    L --> M{LLMProvider in ollama|gemini?}
    M -->|no| K
    L --> N{NotifyProvider in slack|teams?}
    N -->|no| K
    L --> O{MaxIterations >= 1?}
    O -->|no| K
    M -->|yes| P
    N -->|yes| P
    O -->|yes| P[populated Config]
    P --> Q["return Config, nil"]
```

- `loadFrom(lookup)` keeps loading testable without touching the real environment.
- `Validate()` enforces invariants defaults can't (provider enums, `MaxIterations >= 1`).
- See `docs/architecture.md` §12 and `.env.example` for the full variable list.
