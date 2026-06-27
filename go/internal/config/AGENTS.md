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
- `Validate()` enforces invariants defaults can't (provider enums, `MaxIterations >= 1`,
  and **non-empty `REPOS` in App mode** — empty "all repos" is a footgun once an App
  installation can see more repos than intended).
- **GitHub auth mode** (`resolveGitHubApp`): with no `GITHUB_APP_*` vars set, the
  loader stays in PAT mode (`GITHUB_TOKEN`). Once any `GITHUB_APP_*` var is present,
  App mode is intended and requires `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, and
  exactly one of (`GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_PRIVATE_KEY_PATH`); any
  partial/misconfigured App setup is a **startup error**, never a silent fallback to PAT.
  `Config.AppMode()` reports which path is active; the resolved key lives in
  `Config.GitHubApp` (PEM unescaped + validated to parse at load). The static-token vs
  installation-token choice is realized in `internal/auth`.
- See `.agents/standards/architecture-design.md` §12 and `.env.example` for the full variable list.
