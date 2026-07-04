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
| Go (`go/`) | `make ci` | `tidy` → `vet` → architecture tests (`ARCH/`) → `test` → `cover` (≥80%) |
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

CI integration details (what runs where in hosted CI, and how the verify checks that
drive fixer resumes are wired) live in the
[CI integration standard](/standards/ci-integration.md).
