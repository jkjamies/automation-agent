# Testing

How to run **every** kind of test for each port, plus the rules they must obey. This
is the source of truth — read it and you can run the suite without asking anyone.

> **Scope:** the detailed walkthrough below uses the **Go** reference (`go/`); the same test
> kinds and `make` targets apply to every port — run them from that port's directory. Per-port
> drift is tracked in `specs/parity-status.md`.

---

## Principles (all ports)

- **Coverage ≥ 80%**, enforced by `make cover` (and `make ci`). Put the hard logic in
  injectable, LLM-free functions so it's reachable by unit tests.
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
- The *cases* mirror across ports even though the frameworks differ (`testing` vs
  JUnit/kotlin-test vs pytest). See `.agents/standards/language-parity.md`.

---

## Go (`go/`) — reference

Module `github.com/jkjamies/automation-agent`, Go 1.26. **Run everything from the `go/`
directory.** All targets live in `go/Makefile`.

### One-liners

```bash
cd go
make test            # go test ./...                — the whole suite (memory + sqlite backends)
make cover           # tests + 80% coverage gate over ./internal/...
make ci              # tidy + vet + arch + test + cover  (the full local gate)
make arch            # architecture conformance only (import boundaries)
make docs-check      # every directory has an AGENTS.md
make vet             # go vet ./...
make lint            # golangci-lint run
```

`make ci` = `tidy vet arch test cover`, run in order; any failure stops the chain. Run it
before every push — it's the same gate CI enforces.

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
| **Docs conformance** | `ARCH/docs_test.go` | `TestEveryDirHasAgentsDoc` — every directory carries an `AGENTS.md`. |

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

The `*_firestore.go` code is validated against the Firestore **emulator** (needs Java
17+). It is *excluded* from the default `make cover` gate denominator so no one is forced
to run the emulator for everyday work; validate it explicitly:

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
- `make lint` → `golangci-lint run` (config `go/.golangci.yml`): errcheck, govet,
  ineffassign, staticcheck, unused, gofmt, goimports, misspell, revive, **depguard**.
  depguard enforces the same boundary the ARCH tests do — tooling packages (`githubapi`,
  `gitrepo`, `webhook`, `notify`, `scheduler`) may not import `internal/agent/...`. Test
  files are exempt from `errcheck`.

### Running a single test / package

```bash
go test ./internal/agent/fixflow/ -run TestKickoff -v
go test ./internal/agent/setup/ -run TestParkStoreConformance/sqlite -count=1
```

---

## Other ports

Each port has the same `make` targets as Go (`make test`, `make cover`, `make arch`,
`make ci`), run from its own directory. The frameworks differ:

| Port | Framework | Coverage tool | Run the suite |
|---|---|---|---|
| Go `go/` | `go test` | `go tool cover` | `cd go && make ci` |
| Python `python/` | pytest (`pytest-asyncio`) | coverage.py (`pytest --cov`) | `cd python && make ci` |
| TypeScript `javascript/` | the port's runner | the port's coverage | `cd javascript && make ci` |
| Kotlin `kotlin/` | Kotest / JUnit | Kover | `cd kotlin && make ci` (or `./gradlew check`) |

### Python (`python/`)

```bash
cd python
make ci              # ruff + mypy + arch + pytest + coverage gate (>= 80%)
make test            # pytest -q
make cover           # pytest --cov (firestore park store omitted — emulator-only)
make cover-firestore # the firestore-backed tests against a running emulator
```

`SESSION_BACKEND` selects the durable store the suspend/resume tests exercise: `memory`
(default), `sqlite` (adk `SqliteSessionService` + an `aiosqlite` park store), or `firestore`
(adk's native `FirestoreSessionService` + a custom park store on `google-cloud-firestore`).
The firestore park store is emulator-only, so `make cover` omits it from the gate and the
firestore conformance tests are skipped unless `FIRESTORE_EMULATOR_HOST` is set:

```bash
gcloud emulators firestore start --host-port=localhost:8765 &
FIRESTORE_EMULATOR_HOST=localhost:8765 make cover-firestore
```
