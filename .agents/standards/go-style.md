# Go style

Idiomatic Go, matching the surrounding code.

- **Formatting:** `gofmt`/`goimports` clean (`make fmt`); `goimports` local prefix
  is `automation-agent`.
- **Errors:** return errors, don't panic in library code. Wrap with context:
  `fmt.Errorf("doing x: %w", err)`. Handle or explicitly ignore — `errcheck` is on.
- **Naming:** short, lower-case package names; no stutter (`config.Config`, not
  `config.ConfigStruct`). Exported identifiers have doc comments.
- **Dependency injection:** pass collaborators as interfaces/structs (a `Deps`
  struct for agents) so they can be faked in tests. No global mutable state.
- **Configuration:** only `internal/config` reads the environment.
- **Context:** accept `context.Context` as the first parameter for anything doing
  I/O or that can be cancelled.
- **Small packages with one responsibility.** If a package needs an agent import to
  do its job, it's in the wrong layer (see `architecture.md`).
