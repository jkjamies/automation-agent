# internal/config

Single source of truth for runtime configuration. Loaded once from the environment
(`config.Load`) and passed down; **no other package reads `os.Getenv`**.

- `loadFrom(lookup)` keeps loading testable without touching the real environment.
- `Validate()` enforces invariants defaults can't (provider enums, `MaxIterations >= 1`).
- See `docs/architecture.md` §12 and `.env.example` for the full variable list.
