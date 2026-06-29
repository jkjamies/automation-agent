# config

Single source of truth for runtime configuration: loaded once from the environment
(`Config.load()`) and passed down; **no other package reads the environment**.

For the full variable list see `.agents/standards/architecture-design.md` §12 and `.env.example`.

## Details

- `Config.kt` holds `Config` (data class), `Provider`/`NotifyProvider` (enums), `load()`,
  and the testable `loadFrom(Lookup)` — the `Lookup` fun-interface resolves an environment
  key to its value (null = unset).
- Provider/notify validity is enforced at load (invalid values throw
  `IllegalArgumentException`); `validate()` covers the remaining `maxIterations >= 1`
  invariant and the App-mode REPOS allowlist requirement.
- `CI_TIMEOUT` and `TASKS_DISPATCH_DEADLINE` are parsed by a small duration parser (`90m`,
  `1h30m`, …) into `kotlin.time.Duration`.
- **Execution transport** (`TASKS_BACKEND`): `inprocess` (default) needs nothing; `cloudtasks`
  **fails fast** at load unless `TASKS_PROJECT` (or `GOOGLE_CLOUD_PROJECT`), `TASKS_LOCATION`,
  `TASKS_QUEUE`, an absolute **https** `DISPATCH_URL` ending in `/internal/dispatch` (`http://` would
  leak the Bearer; `isSecureDispatchUrl`), `INTERNAL_TOKEN`, and `GITHUB_WEBHOOK_SECRET` are all set,
  and `TASKS_DISPATCH_DEADLINE` is within Cloud Tasks' 15s..30m range (default and ceiling 30m). See
  `tasks/AGENTS.md` and `specs/20260626-workflow-execution-transport.md`.
- **GitHub App mode** (`resolveGitHubApp`): `GITHUB_APP_ID` / `GITHUB_APP_INSTALLATION_ID` plus
  exactly one of `GITHUB_APP_PRIVATE_KEY` / `GITHUB_APP_PRIVATE_KEY_PATH` select the production App
  auth path (`appMode()` → true). Absent App vars leave the zero value = PAT mode; a partial or
  misconfigured set is a **startup error**, never a silent PAT fallback. The ids must be strictly
  positive (a zero App ID would collide with the PAT-mode sentinel). The private key is unescaped
  (CI secret stores flatten newlines to literal `\n` — restored when escaped sequences are present,
  even alongside a real trailing newline) and validated to parse as RSA via `auth.parseRsaPrivateKey`
  (fail-fast at startup, not at the first token exchange). In App mode an empty `REPOS` is rejected
  (an installation can see every repo it is installed on). The resolved id / installation id / PEM are
  consumed by `auth.newAppProvider`; see `auth/AGENTS.md`.
