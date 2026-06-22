# src/config

Single source of truth for runtime configuration. Loaded once from the environment
(`load`) and passed down; **no other module reads `process.env`**.

```mermaid
flowchart TD
    A[caller / main] -->|process.env| B["load()"]
    B -->|"lookup func"| C["loadFrom(get: Lookup)"]
    C --> D["getOr(get, key, default)"]
    D -->|"env set & non-empty"| E[use env value]
    D -->|missing or empty| F[use default]
    C --> G["splitList(REPOS)"]
    C --> I["parseInt(MAX_ITERATIONS)"]
    I -->|parse error| K["throw Error"]
    C --> J["parseDuration(CI_TIMEOUT)"]
    J -->|parse error| K
    C --> L["validate(c)"]
    L --> M{llmProvider in ollama|gemini?}
    M -->|no| K
    L --> N{notifyProvider in slack|teams?}
    N -->|no| K
    L --> O{maxIterations >= 1?}
    O -->|no| K
    M & N & O -->|yes| P[populated Config] --> Q["return Config"]
```

- `config.ts` — `loadFrom(lookup)` keeps loading testable without touching the real
  environment. `parseDuration` supports the duration-unit subset CI_TIMEOUT needs,
  returning milliseconds.
- `validate()` enforces invariants defaults can't (provider enums, `maxIterations >= 1`).
- See `.agents/standards/architecture-design.md` §12 and `.env.example` for the full variable list.
