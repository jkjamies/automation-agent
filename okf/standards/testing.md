---
type: Standard
title: Testing
description: How to run every kind of test and the rules they obey — 80% coverage, no LLM-content assertions, and stubbed networks.
tags: [testing, coverage, conventions]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Testing

How to run **every** kind of test, plus the rules they must obey. This is the source of
truth — read it and you can run the suite without asking anyone. All commands run from the
`go/` directory.

---

## Principles

- **Coverage ≥ 80%**, enforced by `make cover` (and `make ci`) **per package as well as
  overall**. The total alone is an average, and an average hides its laggards: a package can
  sit well under the standard indefinitely while the total reads healthy because the
  well-tested packages outnumber it. Put the hard logic in injectable, LLM-free functions so
  it's reachable by unit tests.
- **Never assert on LLM output content.** LLM responses are non-deterministic; tests that
  check for keywords/tone/persona are flaky by nature. Validate agent *wiring* (with a
  fake `model.LLM`) and *deterministic tooling* instead. Behavior quality is checked
  manually / via eval, not pytest-style content assertions.
- **Test the build-agent pattern:** `Build<Name>Agent` is tested with fakes to assert
  structure; `<name>.go` logic is tested directly.
- **No real network in unit tests.** Stub GitHub, Slack/Teams, and Ollama with in-process
  HTTP servers (`httptest`). Real external services only behind an explicit env gate.
- **Table-driven tests** where they reduce duplication. Name tests for behavior.
- Keep tests in the **same package** for white-box access, or a `_test` package when
  asserting the public API surface.
- **Regression tests pin the bug, not the fix.** A test for a fixed defect must fail
  against the pre-fix code; if it passes either way it documents nothing.

---

## Running the suite

Module `automation-agent`, Go 1.26. **Run everything from the `go/`
directory.** All targets live in `go/Makefile`.

### One-liners

```bash
cd go
make test            # go test ./...                — the whole suite (memory + sqlite backends)
make cover           # tests + 80% coverage gate over ./internal/... (per package and overall)
make ci              # tidy-check + vet + lint + arch + test + cover  (the full local gate)
make arch            # architecture conformance only (import boundaries)
make docs-check      # okf/ bundle conformance (frontmatter, indexes, links)
make vet             # go vet ./...
make lint            # golangci-lint run
```

`make ci` = `tidy-check vet lint arch test cover`, run in order; any failure stops the chain. Run
it before every push — it's the same gate CI enforces.

The gate is **read-only**: it never modifies the tree. `tidy-check` (`go mod tidy -diff`) reports
an untidy `go.mod` instead of tidying it, so the failure names its own cause rather than showing
up as a mystery diff. Run `make tidy` to fix it.

### Test kinds present

| Kind | Where | How it works |
|---|---|---|
| **Unit** | `internal/config`, `internal/ingest`, `internal/agent/.../*_test.go` | Pure functions, deterministic LLM stubs (`fixedLLM`, `stubLLM`, `scriptedLLM`). |
| **HTTP-stub unit** | `internal/githubapi`, `internal/notify`, `internal/webhook`, `setup/ollama_test.go` | `httptest.Server` / `http.ServeMux` fakes GitHub, Slack/Teams, Ollama `/api/chat`. No real network. |
| **Real-git integration** | `internal/gitrepo`, `internal/agent/fixflow` | `seedRemote(t)` builds a temp git repo (go-git) in `t.TempDir()`; clone/commit/push exercised for real. |
| **Suspend/resume integration** | `setup/suspend_resume_test.go`, `setup/longrun_test.go`, `fixflow/engine_test.go` | Deterministic `suspendStub` / sequencer models drive park → wait-for-CI → resume cycles, retries, and timeouts. |
| **Durable cross-process** | `setup/durable_resume_test.go`, `setup/parkstore_test.go` (`TestSQLiteParkStoreCrossProcess`, `TestDurableCrossProcessResume`) | Parks a run on SQLite, tears the runner down, rebuilds from the file, and resumes — proves a restart doesn't strand runs. |
| **Backend conformance suites** | `setup/parkstore_test.go` (`TestParkStoreConformance`), `setup/session_firestore_test.go` | One suite of subtests runs against `memory`, `sqlite`, and (when the emulator is up) `firestore`; the Firestore session suite runs adk's own `session_test.RunServiceTests`. |
| **Architecture conformance** | `ARCH/arch_test.go` | `TestToolingDoesNotImportAgents`, `TestProviderSDKsOnlyInSetup`, `TestNothingImportsCmd` — static import-graph rules. |
| **Docs conformance** | `ARCH/docs_test.go` | `TestOKF*` — the okf/ knowledge bundle conforms: frontmatter `type` on every concept, `index.md` per directory, bundle-absolute links resolve. |

There are **no** benchmarks, fuzz tests, or `//go:build` tags. Optional/slow paths are
gated by env vars instead (below).

### The three storage stacks in tests

The session history **and** the park record (suspend/resume state) are switched by
`SESSION_BACKEND`. Tests cover all three:

| Backend | Selected by | In tests |
|---|---|---|
| `memory` (default) | nothing — the default | Most tests. `session.InMemoryService()` / `NewMemoryParkStore()`. Ephemeral. |
| `sqlite` | `SESSION_BACKEND=sqlite` | Durable-local tests use `t.TempDir()` `.db` files (glebarez/sqlite + gorm). Run automatically by `make test`/`make cover`. |
| `firestore` | `SESSION_BACKEND=firestore` | **Emulator-gated** — skipped unless `FIRESTORE_EMULATOR_HOST` is set (see below). Isolated per-run via timestamped collection prefixes. |

### Firestore-backed tests (emulator)

The `*_firestore.go` code is validated against the Firestore **emulator** (needs a JRE).
It is *excluded* from the default `make cover` gate denominator so no one is forced to run
the emulator for everyday work — which makes running it explicitly load-bearing rather than
optional: without it `internal/agent/setup` measures ~41% instead of ~82%, and the missing
half is the cloud durability path production runs on. CI runs it in its own job.

Either the standalone jar (no gcloud SDK needed — this is what CI uses):

```bash
curl -fsSL -o /tmp/firestore-emulator.jar \
  https://storage.googleapis.com/firebase-preview-drop/emulator/cloud-firestore-emulator-v1.19.8.jar
java -jar /tmp/firestore-emulator.jar --host=127.0.0.1 --port=8765 &
FIRESTORE_EMULATOR_HOST=127.0.0.1:8765 make cover-firestore
```

or via gcloud, if you already have it:

```bash
gcloud components install cloud-firestore-emulator            # one-time
gcloud beta emulators firestore start --host-port=localhost:8085 &
FIRESTORE_EMULATOR_HOST=localhost:8085 GOOGLE_CLOUD_PROJECT=test make cover-firestore
```

`make cover-firestore` runs `go test ./internal/agent/setup/... -run
'Firestore|ParkStoreConformance' -count=1`. Without `FIRESTORE_EMULATOR_HOST` it fails
fast with a hint; the individual tests `t.Skip()` when it's unset.

### Live LLM tests (optional)

Model-touching tests stub the LLM by default. To exercise a **real** local Ollama instead
of the stub, set `OLLAMA_LIVE=1` (and have Ollama running — `make ollama-check`):

```bash
OLLAMA_LIVE=1 go test ./internal/agent/...   # lintfixer, covfixer, summary, setup/ollama
```

These never assert on model *content* — they assert the call wiring round-trips.

### Coverage details

- `make cover` runs `go test -coverprofile=coverage.out -covermode=atomic ./internal/...`
  (`cmd/` is composition-only and excluded), then greps `*_firestore.go:` lines out into
  `coverage.gate.out` and fails if the remaining total is `< 80%`.
- Inspect locally: `go tool cover -func=coverage.out` (per-func) or
  `go tool cover -html=coverage.out` (browser).
- **Race detector** is not in the default gate; run it manually when touching concurrency
  (the park-store has `ConcurrentResolveExactlyOne`-style contention tests):
  `go test -race ./...`.

### Lint / vet alongside tests

- `make vet` → `go vet ./...`.
- `make lint` → golangci-lint (config `go/.golangci.yml`, schema v2): errcheck, govet,
  ineffassign, staticcheck, unused, misspell, revive, **depguard**, plus gofmt/goimports as
  formatters. Note that v2's `staticcheck` subsumes the former gosimple and stylecheck, so
  the `S*` and `ST*` diagnostics are part of the set. depguard enforces the same boundary the
  ARCH tests do — tooling packages (`githubapi`, `gitrepo`, `webhook`, `notify`, `obs`,
  `tasks`) may not import `internal/agent/...`. Test files are exempt from `errcheck`.
- `lint` is **part of `make ci`**, so a new finding fails the gate rather than accumulating.
  The pinned linter is built from source at the module's own Go toolchain (`make
  lint-install`); a released binary refuses a module whose `go` directive is newer than its
  own build.

### Running a single test / package

```bash
go test ./internal/agent/fixflow/ -run TestKickoff -v
go test ./internal/agent/setup/ -run TestParkStoreConformance/sqlite -count=1
```

---

