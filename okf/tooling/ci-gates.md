---
type: Tooling
title: CI gates — the local quality bar per port
description: Every port ships a full local CI gate (lint, typecheck/vet, architecture tests, tests, coverage ≥80%) that must pass before a change is proposed.
tags: [ci, testing, quality-gate]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

Each port owns a self-contained local gate, run from that port's directory:

| Port | Command | What it runs |
|---|---|---|
| Go (`go/`) | `make ci` | `tidy` → `vet` → `lint` (golangci-lint) → architecture tests (`ARCH/`) → `test` → `cover` (≥80%) |
| Python (`python/`) | `make ci` | `ruff` lint → `mypy` typecheck → architecture tests (`arch/`) → `pytest` → coverage (≥80%) |
| TypeScript (`javascript/`) | `make ci` | lint → typecheck → architecture tests (`arch/`) → tests → coverage (≥80%) |
| Kotlin (`kotlin/`) | `./gradlew build` | compile + detekt/ktlint + tests (architecture assertions live in the test suite) |

Shared properties, enforced by the [testing standard](/standards/testing.md) and the
architecture tests:

- **Coverage ≥ 80 %** over the port's library code (composition-only entrypoints
  excluded). Emulator-only backends (e.g. the Firestore session/park stores) are
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
request. It is one job that shells out to the same `make ci`, so the hosted gate cannot
drift from the local one, plus a `git diff --exit-code` afterwards — `make ci` starts with
`go mod tidy` and formats via the lint step, both of which rewrite files in place, so the
check turns silent drift into a failure.

Two things it deliberately does not run: the Firestore-emulator suite
(`make cover-firestore`, which is what folds the `*_firestore.go` cloud backends into
measured coverage) and anything that calls a real model.

**golangci-lint is built from source, pinned, at the module's own Go toolchain.** A
released binary is built against whatever Go its release used, and golangci-lint refuses to
analyze a module whose `go` directive is newer than its own build. `make lint` therefore
installs the pinned version with `GOTOOLCHAIN` read from `go.mod` (so it follows a version
bump automatically) and CI caches the resulting binary. `make lint-install` forces a
reinstall.

CI integration details (what runs where in hosted CI, and how the verify checks that
drive fixer resumes are wired) live in the
[CI integration standard](/standards/ci-integration.md).
