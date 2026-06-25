# Language parity (Go · Kotlin · Python · TypeScript)

`automation-agent` is maintained as **parallel ports of one design** that must remain
**1:1 in functionality**. This document is the contract every port obeys.

## Reference and ports

| Language | Location | ADK | Role |
|---|---|---|---|
| Go | `go/` (`cmd/`, `internal/`) | `google.golang.org/adk` v1.4.0 | **reference (source of truth)** |
| Kotlin | `kotlin/` | `com.google.adk:google-adk-kotlin-core` 0.4.0 ([adk-kotlin](https://github.com/google/adk-kotlin)) | port |
| Python | `python/` | `google-adk` (PyPI) | port |
| TypeScript | `javascript/` | `@google/adk` ([adk-js](https://github.com/google/adk-js)) | port |

Each language targets its **own native ADK** (adk-go, adk-kotlin, adk-python, adk-js), so parity is
**functional, not version-matched** — the ADKs are at different versions and expose
different idiomatic APIs. The shared contract is the *agent topology and behavior*, not the
SDK calls.

The language-neutral design lives in `.agents/standards/architecture-design.md`. When the design and a port
disagree, the design wins; when Go and a port disagree on undocumented behavior, **Go wins**.

## What "1:1" means

Parity is about **observable behavior and structure**, not literal syntax. Idiomatic
language differences are expected and encouraged (coroutines vs goroutines, `Result`/
exceptions vs `error` returns, data classes vs structs). What must match across ports:

1. **Package / directory structure.** Each Go package under `internal/` and `cmd/` maps
   to an equivalent package/module in every port. Same names where the language allows.
2. **Public surface.** The same types, constructors, methods, and their semantics. A
   function that validates and returns an error in Go validates and signals failure the
   idiomatic way in the port — but with the same inputs, outputs, and error conditions.
3. **Configuration.** Identical env var names, defaults, validation rules, and precedence.
4. **External contracts.** Same HTTP routes, request/response shapes, webhook signature
   verification, Slack/Teams payloads, GitHub API calls, labels, and check names. Anything
   another system observes must be byte-compatible where it matters. The webhook routes and
   `check_run` names every port must match are registered in [`webhooks.md`](webhooks.md).
5. **Conventions.** Per-directory `AGENTS.md`; the build-agent pattern (pure wiring split
   from testable logic); prompts as markdown loaded from resources; ≥80% test coverage;
   never assert on LLM output content; provider SDKs confined to the `agent/setup` layer;
   tooling never imports agents.
6. **Docs + diagrams.** The root and `agent/root` `AGENTS.md` diagrams (and the
   `architecture-design.md`/`deployment.md` topology diagrams) must stay consistent across
   ports — when an agent, ingest `Kind`, or ingress route changes, update the parallel
   diagrams in every port in the same change. See [`documentation.md`](documentation.md).

## What may differ

- Build system and dependency manifests (Go modules vs Gradle vs uv/pip).
- Concurrency primitives, error representation, null-handling, and collection idioms.
- Test framework (`testing` vs JUnit/Kotlin-test vs pytest) — but the *cases* should mirror.
- Library choices where Go's pick has no direct equivalent, as long as the contract holds
  (e.g. go-git ↔ JGit, go-github ↔ GitHub's Java client or raw REST).
- **The ADK itself.** Each port uses its language's native ADK at whatever version is
  current; the agent *wiring* differs, the agent *topology and behavior* do not.

## Workflow rule

- **Change Go first.** New behavior or fixes land in the reference, then propagate into
  every *existing* port within the same logical change set. Ports never silently drift.
- **Touch one, check the rest.** A PR that edits any port must either update the others or
  record the deliberate gap in that PR's description. Parity is tracked per-PR: each change
  states which ports it covers and any divergence it knowingly leaves open.
