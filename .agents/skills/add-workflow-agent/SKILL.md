---
name: add-workflow-agent
description: Scaffold a new workflow agent end-to-end on the modern pair — ingest Kind, dispatcher registration, agent package, tests, webhook route, and the knowledge updates. Use when adding an entirely new workflow (a new fixer, digest, or reviewer-style agent) to the system.
---

# Add Workflow Agent

Scaffold a complete new workflow agent: Go reference first, Python mirror in the same
change, knowledge bundle updated before the change is called done.

**Parameters**: agent name (lowercase slug, e.g. `typecheck`), spec via `@file` — **a spec
is required for this skill**. A new workflow fixes routes, check names, and a Kind into
the external contract; do not invent those values from a one-line description.

**Usage examples**:
```text
/add-workflow-agent @specs/20260710-typecheck-fixer.md
/add-workflow-agent secscan @specs/20260712-security-scan-agent.md
```

## Pre-flight: Grill the Spec

Scan the spec for unresolved markers (`<!-- TODO -->`, empty `- [ ]` AC items, empty
required header fields, `...` table cells, `presumably`/`appears to` presumptions). If no spec
was provided, **stop and create one first** (`/ac-to-spec`). If markers are present,
**stop and run `/grill-me @specs/<file>.md` on it first**, then resume. The spec must pin down: the trigger (cron / custom webhook / native GitHub event),
the ingest `Kind`, whether the agent is one-shot or a parking fixer, and (for fixers) the
branch slug and verify check name.

## Reference knowledge (read before writing code)

- What the system is + the four existing workflows: okf/orientation/what-this-system-is.md
- The path an event takes (kickoff vs resume): okf/orientation/event-flow.md
- Wiring/logic split every agent dir must follow: okf/standards/agent-build-pattern.md
- Import boundaries the ARCH suite enforces: okf/standards/architecture.md
- Route / Kind / branch / check-name registry + naming patterns ("Adding a new workflow"): okf/standards/webhooks.md
- Test rules (deterministic, no LLM-content assertions, ≥80%): okf/standards/testing.md
- Go-first / Python-mirror contract: okf/standards/language-parity.md
- Docs-move-with-code rule: okf/standards/documentation.md
- The dispatcher being extended: okf/modules/agents/root-dispatcher.md
- Envelope normalization: okf/modules/platform/ingest.md · HTTP ingress: okf/modules/platform/webhook.md
- Fixer-shaped agents share the engine: okf/modules/agents/fixflow.md

## Steps — Go reference first (`go/`)

### 1. Ingest Kind

`go/internal/ingest/envelope.go` — add `Kind<Name>` and include it in `Valid()`. The wire
codec (`Encode`/`Decode`) is an external contract; an unknown Kind must stay a poison
error (see okf/modules/platform/ingest.md).

### 2. Webhook route (if event-driven)

`go/internal/webhook/handlers.go` + route registration in `server.go`. Pick the door per
okf/standards/webhooks.md: custom kickoff route (`POST /webhooks/<name>`), or fan-out from
`/webhooks/github` by `X-GitHub-Event`. Follow the existing handler shape (size cap, HMAC
when secret set, `ingest.New(Kind<Name>, ...)` → `IngestFunc`, 202). Cron-driven agents
use an `/internal/*` bearer-gated trigger instead.

### 3. Agent package

`go/internal/agent/<name>/`, following okf/standards/agent-build-pattern.md:

- `agents_setup.go` — `Build<Name>Agent(d Deps) (agent.Agent, error)`: ADK wiring only,
  no business logic, no I/O, no env reads.
- `<name>.go` — deterministic behavior: payload parsing, tool `Run` funcs, callbacks.
- `prompts/*.md` — every prompt as markdown loaded from resources, never inline strings.
- **Fixer-shaped agents** skip the wiring file: build a `fixflow.Spec` (triage/analyze fns,
  branch `automation-agent/<slug>`, label `automation-agent`, check `agent-<name>-verify`)
  and expose `NewEngine(fixflow.Deps)` — see okf/modules/agents/fixflow.md.

New env vars go **only** through `go/internal/config` (values arrive in `Deps`).

### 4. Dispatcher registration

`go/internal/agent/root/agents_setup.go` — register the handler for the new Kind,
conditionally on its deps being present (nil deps → not registered, unhandled Kind is a
logged no-op). Wire the real deps in `go/cmd/agent`.

### 5. Tests

`<name>_test.go` beside the code: build-test the wiring with a fake `model.LLM`, unit-test
the logic directly, `httptest` for anything external. Deterministic only — never assert on
LLM output content. The package must clear the ≥80% gate (okf/standards/testing.md).

### 6. Python mirror — same change set

Repeat 1–5 in `python/automation_agent/`: `ingest/envelope.py`, `webhook/`,
`agent/<name>/` (`agents_setup.py` + `<name>.py` + `prompts/`), `agent/root/agents_setup.py`,
`cmd/`. Tests mirror the Go cases in `python/tests/test_<name>.py`. Same env var names,
routes, Kind string, branch, label, check name — byte-identical external contract
(okf/standards/language-parity.md). Do **not** touch `kotlin/` or `javascript/`.

### 7. Knowledge update — MANDATORY

A change is not done until the concepts and diagrams that describe it are updated in the
same change (okf/standards/documentation.md). Run `/update-okf`, which must produce:

- new concept `okf/modules/agents/<name>.md` + entry in `okf/modules/agents/index.md`;
- kickoff (and resume, if parking) added to the okf/orientation/event-flow.md diagrams;
- the okf/modules/agents/root-dispatcher.md flow + behavior lists;
- new rows in **both** okf/standards/webhooks.md tables if any route / Kind / check name
  was added, plus a CI-author example in okf/standards/ci-integration.md for fixers;
- `.env.example` in `go/` and `python/` + the architecture-design config table for any
  new env var.

## Verification

```bash
cd go && make ci            # tidy + vet + arch + test + 80% cover
cd go && make docs-check    # okf bundle conformance
cd python && make ci        # ruff + mypy + arch + pytest + cover
```

Then grep the repo for the new route, Kind, branch, and check name — every mention (code,
concepts, diagrams, registry) must agree.

## Key Rules

- **Go first, Python in the same logical change** — the modern pair never silently drifts;
  any deliberate gap is recorded in the PR description.
- **Frozen pair untouched** — feature work never edits `kotlin/` or `javascript/`.
- **Check names are globally unique** and routes/Kinds/branches are external contracts —
  follow the naming patterns in okf/standards/webhooks.md exactly.
- **Wiring stays pure** — `agents_setup.*` has no logic, no I/O, no env reads; only
  `config` reads the environment; tooling never imports agents.
- **No LLM-content assertions, ever** — fake the model, test wiring and deterministic logic.
- **The bundle update is part of the change**, not a follow-up.
