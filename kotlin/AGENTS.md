# automation-agent — Kotlin port

This is the **Kotlin port** of `automation-agent`. The canonical reference is the **Go**
implementation in [`../go/`](../go/); this port must stay **1:1 in functionality** with it.
Read the language-neutral design in [`../.agents/standards/architecture-design.md`](../.agents/standards/architecture-design.md)
and the parity contract in
[`../.agents/standards/language-parity.md`](../.agents/standards/language-parity.md) first.

Built on [ADK for Kotlin](https://github.com/google/adk-kotlin)
(`com.google.adk:google-adk-kotlin-core`), the coroutine-based native SDK — the counterpart
to `google.golang.org/adk` in Go. Parity is **functional, not version-matched**.

## Layout (mirrors the Go reference)

Package root: `com.automation.agent` under `src/main/kotlin/...`.

| Kotlin package | Go package | Purpose |
|---|---|---|
| `app` | `cmd/agent` | service entrypoint (`Main.kt`) |
| `config` | `internal/config` | env → typed `Config`; sole reader of the environment |
| `ingest` | `internal/ingest` | normalized `Envelope` + `Kind` |
| `notify` | `internal/notify` | Slack/Teams behind one `Notifier` |
| `githubapi` | `internal/githubapi` | GitHub REST tooling |
| `gitrepo` | `internal/gitrepo` | git working-tree tooling |
| `webhook` | `internal/webhook` | HTTP ingress |
| `tasks` | `internal/tasks` | execution transport (in-process \| Cloud Tasks → `/internal/dispatch`) |
| `agent.setup` | `internal/agent/setup` | LLM builder, Ollama adapter, prompt loader, runner |
| `agent.root` | `internal/agent/root` | dispatcher |
| `agent.summary` | `internal/agent/summary` | commit-digest workflow |
| `agent.lintfixer` | `internal/agent/lintfixer` | lint-fix workflow |
| `agent.covfixer` | `internal/agent/covfixer` | coverage-fix workflow |
| `agent.fixflow` | `internal/agent/fixflow` | shared fix engine |

## Conventions (same as the Go reference)

- **Every package directory has an `AGENTS.md`.**
- **Build-agent pattern:** pure wiring (a `build<Name>Agent` function) is split from
  testable logic. See `../.agents/standards/agent-build-pattern.md`.
- **Import boundaries:** tooling (`githubapi`, `gitrepo`, `notify`, `webhook`)
  must not import `agent.*`; provider SDKs (Ollama/Gemini) only in `agent.setup`; `config`
  is the only environment reader.
- **Prompts are markdown** under `src/main/resources/prompts/<agent>/`, loaded from the
  classpath (the `embed.FS` equivalent).
- **Testing:** [Kotest](https://kotest.io) `BehaviorSpec` with `Given`/`When`/`Then` blocks
  (no backtick-named test functions). Test classes/files use the **`Test`** suffix (e.g.
  `OllamaModelTest`), not `Spec`. ≥80% coverage via Kover (`./gradlew koverVerify`). Never
  assert on LLM output.
- **No `!!`.** Never use the not-null assertion operator (except very exceptional test
  cases). Use `shouldNotBeNull()`, `getValue(...)`, `?:`, `requireNotNull(...)`, or
  smart-casts instead.
- **Architecture tests:** [Konsist](https://github.com/LemonAppDev/konsist) in the dedicated
  `:konsist` module, run via `./gradlew arch` (the analogue of Go `make arch` → `ARCH/`).
- **Models:** default to local Ollama Gemma; never hardcode a provider in agents.

## Working here

```bash
./gradlew build           # compile + test (service module)
./gradlew test            # unit tests only (Kotest)
./gradlew arch            # architecture conformance (Konsist; :konsist module)
./gradlew koverVerify     # 80% coverage gate (mirrors Go `make cover`)
./gradlew run             # run the service (mirrors Go `make run`)
```
