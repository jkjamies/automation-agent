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
- `validate()` enforces invariants defaults can't (provider enums, `max_iterations >= 1`,
  and **non-empty `REPOS` in App mode** — empty "all repos" is a footgun once an App
  installation can see more repos than intended).
- **GitHub auth mode** (`_resolve_github_app`): with no `GITHUB_APP_*` vars set, the
  loader stays in PAT mode (`GITHUB_TOKEN`). Once any `GITHUB_APP_*` var is present, App
  mode is intended and requires `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, and exactly
  one of (`GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_PRIVATE_KEY_PATH`); any
  partial/misconfigured App setup is a **startup error**, never a silent fallback to PAT.
  `Config.app_mode()` reports which path is active; the resolved key lives in
  `github_app_private_key_pem` (escaped `\n` unescaped + validated to parse as RSA at
  load). The static-token vs installation-token choice is realized in `auth`.
- See `.agents/standards/architecture-design.md` §12 and `.env.example` for the full variable list.
