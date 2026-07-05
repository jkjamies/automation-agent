---
type: Standard
title: Language parity (Go · Python — modern pair · Kotlin · TypeScript — frozen pair)
description: The cross-language parity contract organizing the four ports into a modern Go/Python pair and a frozen Kotlin/TypeScript pair.
tags: [parity, ports, contract]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Language parity (Go · Python — modern pair · Kotlin · TypeScript — frozen pair)

`automation-agent` is maintained as **parallel ports of one design**, organized as **two
parity pairs** with no parity requirement *across* the pairs. This document is the
contract each pair obeys.

## The two pairs

| Pair | Language | Location | ADK | Role |
|---|---|---|---|---|
| **Modern** | Go | `go/` (`cmd/`, `internal/`) | `google.golang.org/adk/v2` (2.x line) | **reference (source of truth)** |
| **Modern** | Python | `python/` | `google-adk` (PyPI, 2.x line) | port of the reference |
| **Frozen** | Kotlin | `kotlin/` | `com.google.adk:google-adk-kotlin-core` 0.4.0 ([adk-kotlin](https://github.com/google/adk-kotlin)) | frozen — 1:1 with TypeScript |
| **Frozen** | TypeScript | `javascript/` | `@google/adk` ([adk-js](https://github.com/google/adk-js)) | frozen — 1:1 with Kotlin |

- **The modern pair (Go ↔ Python)** carries the design forward on the ADK 2.x line
  (graph workflows, request-input pause/resume). Full parity contract below.
- **The frozen pair (Kotlin ↔ TypeScript)** is feature-frozen at its current 1:1
  behavior (their ADKs have no 2.x line). It receives no new features; if a critical fix
  ever touches one, it must land in **both** — the pair keeps parity with each other.
- **Across pairs there is no parity requirement.** The modern pair diverges freely from
  the frozen pair's mechanics (e.g. the fixers' CI-wait loop is a workflow graph on the
  modern pair and long-running tools on the frozen pair). External contracts (webhooks,
  check names, notify payloads) still match across all four, because outside systems
  observe them.

Each language targets its **own native ADK**, so parity is **functional, not
version-matched** — the ADKs are at different versions and expose different idiomatic
APIs. The shared contract is the *agent topology and behavior*, not the SDK calls.

## Why two pairs

The ADKs diverged: only the Go and Python lines carry 2.x capabilities (graph
workflows, request-input pause/resume). Four-way parity therefore capped the design at
the least capable SDK and priced every change at four implementations plus four review
passes. Splitting into pairs lets the modern pair carry the design forward at full SDK
capability while the frozen pair — complete, tested, and proving the design on a second
SDK generation — costs nothing to keep. Deleting the frozen ports was rejected for the
same reason freezing is cheap: they still demonstrate the external contract is
language-neutral.

The language-neutral design lives in
[Automation Agent — Architecture & Build Plan](/standards/architecture-design.md) and
describes the modern pair. When the design and a port disagree, the design wins; when Go
and Python disagree on undocumented behavior, **Go wins**.

## What "1:1" means (within a pair)

Parity is about **observable behavior and structure**, not literal syntax. Idiomatic
language differences are expected and encouraged (coroutines vs goroutines, `Result`/
exceptions vs `error` returns, data classes vs structs) — including where the two SDKs
surface the same capability under different helpers, as long as the observable behavior
matches. What must match across a pair:

1. **Package / directory structure.** Each Go package under `internal/` and `cmd/` maps
   to an equivalent package/module in the pair's port. Same names where the language allows.
2. **Public surface.** The same types, constructors, methods, and their semantics. A
   function that validates and returns an error in Go validates and signals failure the
   idiomatic way in the port — but with the same inputs, outputs, and error conditions.
3. **Configuration.** Identical env var names, defaults, validation rules, and precedence.
4. **External contracts.** Same HTTP routes, request/response shapes, webhook signature
   verification, Slack/Teams payloads, GitHub API calls, labels, and check names. Anything
   another system observes must be byte-compatible where it matters. The webhook routes and
   `check_run` names every port must match are registered in
   [Webhooks & CI check names](/standards/webhooks.md).
   **External contracts hold across all four ports**, frozen pair included.
5. **Conventions.** The build-agent pattern (pure wiring split
   from testable logic); prompts as markdown loaded from resources; ≥80% test coverage;
   never assert on LLM output content; provider SDKs confined to the `agent/setup` layer;
   tooling never imports agents.
6. **Docs + diagrams.** The knowledge bundle documents one system, so a behavior change
   updates its concept (and the [event flow](/orientation/event-flow.md) /
   architecture-design / deployment diagrams) **once** — record any deliberate per-port
   delta in the [port concepts](/modules/ports/index.md). See
   [Documentation & diagrams](/standards/documentation.md).

## What may differ (within a pair)

- Build system and dependency manifests (Go modules vs uv/pip).
- Concurrency primitives, error representation, null-handling, and collection idioms.
- Test framework (`testing` vs pytest) — but the *cases* should mirror.
- Library choices where Go's pick has no direct equivalent, as long as the contract holds
  (e.g. go-git ↔ GitPython, go-github ↔ PyGithub).
- **The ADK itself.** Each port uses its language's native ADK at whatever version is
  current; the agent *wiring* differs, the agent *topology and behavior* do not.

## Workflow rule

- **Change Go first.** New behavior or fixes land in the reference, then propagate into
  Python within the same logical change set. The modern pair never silently drifts.
- **Touch one, check the pair.** A PR that edits either modern port must either update the
  other or record the deliberate gap in that PR's description. Parity is tracked per-PR:
  each change states which ports it covers and any divergence it knowingly leaves open.
- **The frozen pair is not touched** by feature work. A critical fix that must land there
  lands in both frozen ports together, preserving their mutual 1:1.
