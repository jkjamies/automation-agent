# The build-agent pattern

Every agent directory (`root/`, `summary/`, `lintfixer/`) splits into two files so
that wiring is separate from logic and everything stays testable.

## `agents_setup.go` — pure wiring

- Exposes one constructor: `Build<Name>Agent(d Deps) (agent.Agent, error)`.
- Assembles ADK constructs only (`llmagent.New`, `sequentialagent.New`,
  `parallelagent.New`, `loopagent.New`) from injected dependencies.
- **No business logic, no I/O, no env reads.** Dependencies arrive via a `Deps`
  struct (the LLM, tooling clients, prompts, config values).

## `<name>.go` — behavior

- The deterministic functions: code-agent `Run` funcs, tool implementations,
  callbacks, payload parsing, correlation handling.
- Plain Go, unit-tested directly without an LLM.

## Why

Tests construct `Build<Name>Agent` with a fake `model.LLM` and fake tooling to
assert structure, and exercise `<name>.go` logic in isolation. This is what makes
the ≥80% coverage target attainable without testing LLM output.

## Shared utilities

Common helpers (LLM builder, prompt loader, event helpers, runner) live in
`internal/agent/setup` so agent dirs stay small.
