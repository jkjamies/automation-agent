# The build-agent pattern

The standalone agent directories (`root/`, `summary/`) split into two files so that wiring
is separate from logic and everything stays testable. The `lintfixer/` and `covfixer/`
fixers follow a different shape — they build a `fixflow.Spec` and expose
`NewEngine(fixflow.Deps)`, sharing the generic `fixflow` engine rather than carrying their
own `agents_setup.*` wiring file (see [the fixflow note](#fixflow-based-fixers) below).

## `agents_setup.go` — pure wiring

- Exposes one constructor: `Build<Name>Agent(d Deps) (agent.Agent, error)`
  (`BuildRootDispatcher` / `BuildSummaryAgent` in Go).
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

## Fixflow-based fixers

`lintfixer/` and `covfixer/` do not use the `Build<Name>Agent` wiring file. Each one builds
a `fixflow.Spec` (branch/label/check names plus its triage/analyze steps) and exposes
`NewEngine(d fixflow.Deps) *fixflow.Engine`. The shared `fixflow` engine carries the
suspend/resume Driver and the triage→analyze→commit→PR→await-CI loop, so the fixers stay
thin. The same testability split still holds: the deterministic logic (`triage.go`,
`analyze.go`, the `Spec` builder) is unit-tested directly without an LLM.

## Shared utilities

Common helpers (LLM builder, prompt loader, event helpers, runner) live in
`internal/agent/setup` so agent dirs stay small.
