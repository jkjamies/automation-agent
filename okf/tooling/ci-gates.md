---
type: Tooling
title: CI gates — the quality bar every change clears
description: The local gate (tidy, vet, lint, architecture tests, tests, coverage ≥80%) and the hosted CI that runs it, both of which a change must pass.
tags: [ci, testing, quality-gate]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

One self-contained local gate, run from `go/`:

| Command | What it runs |
|---|---|
| `make ci` | `tidy` → `vet` → `lint` (golangci-lint) → architecture tests (`ARCH/`) → `test` → `cover` (≥80%) |

Properties, enforced by the [testing standard](/standards/testing.md) and the architecture
tests:

- **Coverage ≥ 80 %** over `internal/` (the composition-only entrypoint is excluded). Emulator-only backends (e.g. the Firestore session/park stores) are
  exercised by separate emulator-gated targets (`make cover-firestore`), not the default
  unit run.
- **Never assert on LLM output content** — tests assert deterministic state transitions,
  not model phrasing.
- **Architecture tests are part of the gate**: import boundaries (tooling never imports
  agents; provider SDKs confined to the setup layer; nothing imports `cmd`) and the
  OKF bundle conformance check (every concept has a `type`, every directory has an
  `index.md`, bundle-absolute links resolve).
- **Docs gates**: `make docs-check` (where present) runs the documentation-related
  architecture tests on demand.

## Hosted CI

`.github/workflows/ci.yml` runs the Go gate on every push to `main` and every pull
request, in two jobs.

**`go`** shells out to the same `make ci`, so the hosted gate cannot drift from the local
one, plus a `git diff --exit-code` afterward.

That diff check is a **backstop**, not a gate on any known rewrite: `make ci` is read-only
end to end. `tidy-check` (`go mod tidy -diff`) reports an untidy `go.mod` without touching
it, and `golangci-lint run` *reports* gofmt/goimports violations as findings rather than
applying them (only `--fix` writes) — so unformatted code fails the lint step outright,
naming the file and line, instead of being silently rewritten and caught a step later. The
check earns its place by catching anything that modifies the tree *unexpectedly*: a step
added later, or a tool that starts writing where it used to only read.

`make fmt` stays a manual convenience, never a gate step. A gate that edits the tree it is
measuring can report success on input it just changed.

**`firestore`** starts the standalone Firestore emulator (the Firebase jar, so no gcloud
SDK — just the JRE the runner already has) and runs `make cover-firestore`. Without it the
cloud session and park-store backends are skipped entirely rather than failing, which is a
quiet gap: `internal/agent/setup` measures ~82% with the emulator and ~41% without, and the
missing half is precisely the durability path production depends on.

Still not run in CI: anything that calls a real model.

**golangci-lint is built from source, pinned, at the module's own Go toolchain.** A
released binary is built against whatever Go its release used, and golangci-lint refuses to
analyze a module whose `go` directive is newer than its own build. `make lint` therefore
installs the pinned version with `GOTOOLCHAIN` read from `go.mod` (so it follows a version
bump automatically) and CI caches the resulting binary. `make lint-install` forces a
reinstall.

CI integration details (what runs where in hosted CI, and how the verify checks that
drive fixer resumes are wired) live in the
[CI integration standard](/standards/ci-integration.md).
