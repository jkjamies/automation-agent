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
- `validate()` enforces invariants defaults can't (provider enums, `maxIterations >= 1`,
  and **non-empty `REPOS` in App mode** — empty "all repos" is a footgun once an App
  installation can see more repos than intended).
- **GitHub auth mode** (`resolveGithubApp`): with no `GITHUB_APP_*` vars set, the loader
  stays in PAT mode (`GITHUB_TOKEN`). Once any `GITHUB_APP_*` var is present, App mode is
  intended and requires `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, and exactly one of
  (`GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_PRIVATE_KEY_PATH`); any partial/misconfigured App
  setup is a **startup error**, never a silent fallback to PAT. `appMode(cfg)` reports which
  path is active; the resolved key lives in `githubAppPrivateKeyPem` (escaped `\n` unescaped
  + validated to parse as RSA at load). The static-token vs installation-token choice is
  realized in `auth` (`buildTokenProvider` in `cmd/agent/main.ts`).
- See `.agents/standards/architecture-design.md` §12 and `.env.example` for the full variable list.
