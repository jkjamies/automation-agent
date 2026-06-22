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
  invariant.
- `CI_TIMEOUT` is parsed by a small duration parser (`90m`, `1h30m`, …) into
  `kotlin.time.Duration`.
