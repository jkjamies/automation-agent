---
name: add-platform-package
description: Scaffold a new deterministic, agent-free tooling package, with tests and its knowledge concept. Use when the system needs a new platform capability (an API client, ingress, transport, notifier-style package) rather than a new workflow.
---

# Add Platform Package

Scaffold a deterministic tooling package in the same
change, concept + index entry in the bundle before the change is called done.

**Parameters**: package name (lowercase, e.g. `ratelimit`), optional spec via `@file` or
inline description of the package's surface.

**Usage examples**:
```text
/add-platform-package @specs/20260715-rate-limiter.md
/add-platform-package cache  # in-memory + firestore-backed response cache for githubapi
```

## Pre-flight: Grill the Spec

When a spec is given via `@file`, scan it for unresolved markers (`<!-- TODO -->`, empty
`- [ ]` AC items, empty required header fields, `...` table cells, `presumably`/`appears
to` presumptions). If any are present, **stop and run `/grill-me @{spec}` first**, then
resume. Bare invocations are fine for small packages — but the public surface (types,
constructors, error conditions) must be settled before writing code: it is the contract
every consumer is built against, and widening it later is far cheaper than narrowing it.

## Reference knowledge (read before writing code)

- Import boundaries the ARCH suite enforces: okf/standards/architecture.md
- What "platform" means (deterministic, agent-free): okf/orientation/what-this-system-is.md
- Existing package concepts to match in shape: okf/modules/platform/index.md
- Test rules (httptest stubs, ≥80%, table-driven): okf/standards/testing.md
- Docs-move-with-code rule: okf/standards/documentation.md
- Go style conventions: okf/standards/go-style.md

## Steps

### 1. Create the package

`go/internal/<name>/` — deterministic code only. The boundaries (enforced by `go/ARCH/`
and depguard, per okf/standards/architecture.md):

- **Never import `internal/agent/...`** — agents call tooling, never the reverse.
- **Never read the environment** — only `internal/config` does; values arrive as
  constructor parameters or an options struct.
- **No provider/LLM SDKs** — those are confined to `internal/agent/setup`.
- Dependencies (HTTP client, clock, logger) are injected so tests can stub them.

### 2. Enforce the boundary on the new package

Add `internal/<name>` to the depguard tooling list in `go/.golangci.yml` (the packages
forbidden from importing agents) so the linter enforces what the ARCH test checks.

### 3. Tests

`<name>_test.go` beside the code: pure-function unit tests plus `httptest.Server` fakes
for any external service — no real network (okf/standards/testing.md). Table-driven where
it reduces duplication; must clear the ≥80% gate.

### 4. Wire consumers

Construct the package in `go/cmd/agent` (entrypoints are leaves — nothing imports `cmd/`)
and hand it to whatever `Deps` struct needs it. Any new env var: add it to
`go/internal/config` with the same name, default, and validation the spec fixed.

### 5. Knowledge update — MANDATORY

A change is not done until the concepts that describe it are updated in the same change
(okf/standards/documentation.md). Run `/update-okf`, which must produce:

- new concept `okf/modules/platform/<name>.md` (house frontmatter, factual, self-contained,
  Mermaid flow if the package has one) + entry in `okf/modules/platform/index.md`;
- okf/orientation/event-flow.md and the architecture-design diagrams **only if** the
  package changes the event path or topology;
- the repo-root `.env.example` + the architecture-design config table for any
  new env var.

## Verification

```bash
cd go && make ci            # tidy + vet + arch + test + 80% cover
cd go && make lint          # depguard picks up the new boundary entry
cd go && make docs-check    # okf bundle conformance
```

## Key Rules

- **Tooling never imports agents** — if the package needs agent types, it is not a
  platform package; reshape the design.
- **Only `config` reads env** — a `os.Getenv` in the new package is a boundary violation
  even if convenient.
- **Same public surface on both modern ports** — same names, inputs, outputs, and error
  conditions; idiom differs, behavior does not.
- **Concept + index entry ship in the same change** — the platform index is how the next
  reader discovers the package exists.
