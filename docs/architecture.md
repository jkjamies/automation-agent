# Automation Agent — Architecture & Build Plan

> Status: **Implemented.** This is the living design doc; the design below is built —
> Phases 1–5 are implemented and `make ci` is green.
> Last updated: 2026-06-21.

A single long-running Go service that ingests events from many sources, routes every
ingest through a **Root Agent**, and runs three workflow agents: a **Summary** workflow
(daily/weekly commit digests), a **Lint-fixer** workflow (autonomous lint remediation
with PR + CI feedback loop), and a **Coverage-fixer** workflow (autonomous test-coverage
remediation). Lint-fixer and Coverage-fixer share a generic `fixflow` engine.

Local-first on **Ollama + Gemma**, with a clean switch to **Gemini/Vertex** for the
persistent GCP deployment — both behind one `model.LLM` builder.

---

## Table of contents

1. [Goals](#1-goals)
2. [Architecture at a glance](#2-architecture-at-a-glance)
3. [Dependencies](#3-dependencies)
4. [Model strategy](#4-model-strategy--one-builder-two-providers)
5. [Repository layout](#5-repository-layout)
6. [The build-agent pattern](#6-the-build-agent-pattern)
7. [The three agents](#7-the-three-agents)
8. [Suspend / resume design (CI feedback loop)](#8-suspend--resume-design-ci-feedback-loop)
9. [Prompts as markdown](#9-prompts-as-markdown)
10. [ARCH tests, AGENTS.md, specs, Makefile](#10-arch-tests-agentsmd-specs-makefile)
11. [Testing & coverage](#11-testing--coverage)
12. [Configuration](#12-configuration)
13. [Deployment](#13-deployment)
14. [Phased roadmap](#14-phased-roadmap)
15. [Open questions](#15-open-questions)
16. [Verified ADK-Go API reference](#16-verified-adk-go-api-reference)

---

## 1. Goals

1. **Ingest events** from many possible sources. Today: cron at **09:00 daily** and
   **09:00 Mondays**. Tomorrow: GitHub / Jira / Confluence / human-triggered. Every
   ingest is normalized into one envelope and handed to the **Root Agent**.
2. **Root Agent** is the universal dispatcher — it inspects the envelope and kicks off
   the right workflow agent(s). Keeping a single entry point is why the root agent exists.
3. **Summary workflow** — fan out over **N configured repos** (parallel), pull the last
   24h of commits per repo (deterministic), feed the aggregate into a reasoning LLM that
   writes a digest, and post it to **Slack or Teams**.
4. **Lint-fixer workflow** — receive a platform-agnostic lint payload, reason about each
   problem, check out the repo, make the change, open a PR, **suspend**, then **resume**
   when a CI webhook reports back — looping up to **3 times**, finishing with a Slack/Teams
   summary (success, or "needs human review" + PR link).
5. **Coverage-fixer workflow** — the same suspend/resume loop applied to test coverage:
   take a coverage report, generate tests, open a PR, and loop on the coverage CI check.
   Lint-fixer and Coverage-fixer share the generic `fixflow` engine.

Non-goals (for now): interactive chat UI, multi-tenant auth. The design must not *preclude*
future human interaction or additional hooks — hence the root-agent indirection.

---

## 2. Architecture at a glance

```
                         ┌──────────────── ingest sources ─────────────────┐
   cron 09:00 daily ─┐   │  webhook: /ci   webhook: /ingest   (future: Jira)│
   cron 09:00 Mon  ──┼──▶│        scheduler + HTTP server                   │
   (future hooks) ───┘   └───────────────────────┬─────────────────────────┘
                                                  ▼
                                       ingest.Envelope (normalized)
                                                  ▼
                                          ┌───────────────┐
                                          │  ROOT AGENT   │  (dispatcher)
                                          └───────┬───────┘
                          ┌───────────────────────┼───────────────────────┐
                          ▼                       ▼                        ▼
              ┌────────────────────┐   ┌──────────────────────┐  ┌──────────────────────┐
              │  SUMMARY workflow   │   │  LINT-FIXER workflow  │  │ COVERAGE-FIXER workflow│
              │ Sequential:         │   │  (fixflow Spec)       │  │  (fixflow Spec)        │
              │  Parallel[fetch×N]  │   │   apply_fix(git/PR)   │  │   apply_fix(git/PR)    │
              │   → summarize(LLM)  │   │   → await_ci (suspend)│  │   → await_ci (suspend) │
              │   → notify          │   │   → resume (webhook / │  │   → resume (webhook /  │
              └─────────┬──────────┘   │      CI_TIMEOUT timer) │  │      CI_TIMEOUT timer) │
                        ▼              │   → notify(summary)    │  │   → notify(summary)    │
                  Slack / Teams        └───────────┬───────────┘  └───────────┬───────────┘
                                                   ▼                          ▼
                                             Slack / Teams              Slack / Teams
```

Lint-fixer and Coverage-fixer share the generic `fixflow` engine; each is a thin `Spec`
(branch/label/check + triage/analyze) over it.

Tooling (`gitrepo`, `githubapi`, `webhook`, `notify`, `scheduler`) is **deterministic
and agent-free** — agents call it, it never imports agents. This boundary is enforced by
ARCH tests.

---

## 3. Dependencies

All verified on pkg.go.dev. `gh` CLI is **not** a dependency — go-github + go-git cover
everything in-process.

| Concern | Library | Notes |
|---|---|---|
| Agent framework | `google.golang.org/adk` | v1.x; agents, workflow agents, runner, model interface |
| Local LLM | `github.com/ollama/ollama/api` | native typed client; `Chat(ctx, *ChatRequest, fn)` |
| Cloud LLM | `google.golang.org/adk/model/gemini` | prod path |
| Cron | `github.com/robfig/cron/v3` | `0 9 * * *` daily, `0 9 * * 1` Mondays |
| HTTP | `net/http` (`ServeMux`, Go 1.22 method routing) | stdlib is enough; chi only if we outgrow it |
| GitHub API | `github.com/google/go-github` | list commits, create PR |
| Git working tree | `github.com/go-git/go-git/v5` | clone/branch/commit/push (pure Go) |
| Arch tests | `github.com/matthewmcnew/archtest` or hand-rolled `go/packages` | import-boundary assertions |
| Lint | `golangci-lint` (incl. `depguard`) | quality gate |
| In-flight state | *(in-memory parked-run registry)* | in-flight runs (PR key → session/call id + attempt count) live only in memory; a per-run `CI_TIMEOUT` timer bounds each wait. GitHub holds the durable PR artifacts (PR + label + check/SHA) but is **not** consulted to recover in-flight state. A process restart strands parked runs (accepted trade-off; crash recovery is out of scope). A DB is a scale-*out* / crash-recovery concern, neither of which is in scope today |

---

## 4. Model strategy — one builder, two providers

`internal/agent/setup` exposes a single builder; every agent receives a `model.LLM`,
never a concrete provider.

```go
// internal/agent/setup/llm.go
type Provider string
const (ProviderOllama Provider = "ollama"; ProviderGemini Provider = "gemini")

// Build returns a model.LLM chosen by config (LLM_PROVIDER / OLLAMA_MODEL / GEMINI_MODEL).
func BuildLLM(cfg LLMConfig) (model.LLM, error)
```

```go
// internal/agent/setup/ollama.go — implements model.LLM
type OllamaModel struct { client *api.Client; model string }

func (m *OllamaModel) Name() string { return m.model }

func (m *OllamaModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
    // 1. convert []*genai.Content -> []api.Message
    // 2. client.Chat(ctx, &api.ChatRequest{Model: m.model, Messages: ...}, yieldFn)
    // 3. convert api.ChatResponse -> *model.LLMResponse (Partial / TurnComplete)
}
```

This adapter is the single most important piece of new infrastructure; it gets its own
unit tests against a stubbed Ollama HTTP server (`httptest`). Default local model:
`gemma4:12b` (balance), `gemma4:e4b` (speed), `gemma4:26b` (quality).

> **Prod tension:** Ollama needs a host. On GCP either (a) run Ollama on a GPU VM, or
> (b) flip `LLM_PROVIDER=gemini` and use Vertex. The builder makes this a config flag,
> not a code change — but it is a real cost/infra decision for later.

---

## 5. Repository layout

```
automation-agent/
├── AGENTS.md                      # repo root: what this is, how to navigate
├── README.md
├── Makefile
├── go.mod / go.sum
├── .gitignore                     # contains: /specs/  .env  /tmp/
├── .env.example
├── .golangci.yml
│
├── cmd/agent/
│   ├── main.go                    # wire config→tooling→runner→scheduler→http; block
│   └── AGENTS.md
│
├── ARCH/                          # architecture tests (its own package)
│   ├── arch_test.go               # import-boundary rules
│   ├── docs_test.go               # assert AGENTS.md presence per dir
│   └── AGENTS.md
│
├── .agents/                       # open-standard knowledge for agents
│   ├── AGENTS.md
│   ├── skills/                    # reusable task recipes (e.g. add-workflow-agent.md)
│   │   └── AGENTS.md
│   ├── standards/                 # the rules of the codebase
│   │   ├── go-style.md
│   │   ├── testing.md             # 80% rule, no-LLM-assert rule
│   │   ├── agent-build-pattern.md # the setup-vs-logic split
│   │   ├── architecture.md        # the import boundaries ARCH enforces
│   │   └── AGENTS.md
│   └── templates/                 # spec templates
│       ├── add.spec.md
│       ├── remove.spec.md
│       ├── change.spec.md
│       ├── migrate.spec.md
│       └── AGENTS.md
│
├── specs/                         # GITIGNORED — developer memory, one file per change
│   └── .gitkeep
│
├── docs/
│   └── architecture.md            # this document
│
└── internal/
    ├── AGENTS.md
    ├── config/                    # env → typed Config; one source of truth
    │   ├── config.go
    │   └── AGENTS.md
    ├── ingest/                    # the normalized Envelope + Kind enum (cron/ci/github/jira…)
    │   ├── envelope.go
    │   └── AGENTS.md
    ├── agent/
    │   ├── AGENTS.md              # SHARED agent doc: explains setup.go vs <name>.go convention
    │   ├── setup/                 # common agent utilities
    │   │   ├── llm.go             # BuildLLM (provider switch)
    │   │   ├── ollama.go          # model.LLM adapter
    │   │   ├── gemini.go          # gemini factory
    │   │   ├── prompt.go          # embed.FS prompt loader -> GetPrompt("summary/summarize")
    │   │   ├── events.go          # helpers to emit/parse session.Event + text
    │   │   ├── runner.go          # build adk Runner + session service
    │   │   └── AGENTS.md
    │   ├── root/
    │   │   ├── agents_setup.go    # BuildRootAgent(deps) -> agent.Agent
    │   │   ├── root.go            # dispatch logic (Run func / callbacks), testable
    │   │   ├── prompts/root.md
    │   │   └── AGENTS.md
    │   ├── summary/
    │   │   ├── agents_setup.go    # BuildSummaryAgent(deps) -> Sequential[Parallel[fetch×N]→sum→notify]
    │   │   ├── summary.go         # fetch code-agent + summarize logic
    │   │   ├── prompts/summarize.md
    │   │   ├── tasks/             # (optional) per-step helpers
    │   │   └── AGENTS.md
    │   ├── lintfixer/             # the lint Spec of the fixflow engine
    │   │   ├── lint.go            # builds the lint Spec (branch/label/check + triage/analyze)
    │   │   ├── prompts/
    │   │   └── AGENTS.md
    │   ├── covfixer/              # the coverage Spec of the fixflow engine
    │   │   ├── coverage.go        # builds the coverage Spec
    │   │   ├── prompts/
    │   │   └── AGENTS.md
    │   └── fixflow/               # generic fix engine shared by lint + coverage
    │       ├── engine.go          # Spec-driven engine (triage→analyze→commit→PR)
    │       ├── driver.go          # suspend/resume Driver (Kickoff/Resume/onTimeout)
    │       ├── registry.go        # in-memory parked-run registry (the in-flight record)
    │       ├── applyfix.go        # one fix attempt: checkout/edit/commit/push/PR
    │       ├── analyze.go         # analyze step
    │       ├── explore.go         # repo exploration helper
    │       ├── tools.go           # apply_fix + long-running await_ci tools
    │       ├── files.go
    │       ├── util.go
    │       ├── envelope.go
    │       └── AGENTS.md
    ├── githubapi/                 # go-github: ListCommits, CreatePR, check status
    │   ├── client.go
    │   └── AGENTS.md
    ├── gitrepo/                   # go-git: Clone, Branch, Commit, Push
    │   ├── repo.go
    │   └── AGENTS.md
    ├── webhook/                   # http.Server + handlers (daily/weekly/ci/ingest)
    │   ├── server.go
    │   ├── handlers.go
    │   └── AGENTS.md
    ├── scheduler/                 # robfig/cron wrapper -> emits ingest.Envelope
    │   ├── scheduler.go
    │   └── AGENTS.md
    └── notify/                    # Slack/Teams behind one interface
        ├── notify.go              # Notifier interface
        ├── slack.go
        ├── teams.go               # plan for Workflows/Adaptive Card (O365 connectors deprecating)
        └── AGENTS.md
```

In-flight suspend/resume state lives in the `fixflow` package's **in-memory parked-run
registry** (`registry.go`) — there is no separate recovery package and no PR-scan ticker.
Resume is webhook-driven, with a per-run `CI_TIMEOUT` timer as the catch-all.

---

## 6. The build-agent pattern

The testability backbone. Strict split inside every agent directory:

- **`agents_setup.go`** — *pure wiring*. One `Build<Name>Agent(deps Deps) (agent.Agent, error)`.
  Only assembles ADK constructs (`llmagent.New`, `sequentialagent.New`, …) from injected
  dependencies. No business logic, no I/O.
- **`<name>.go`** — *behavior*. Deterministic functions: tool implementations, `Run` funcs
  for code-agents, callbacks, payload parsing, correlation handling. Plain Go, unit-tested
  directly with no LLM.

```go
// summary/agents_setup.go
type Deps struct {
    LLM    model.LLM
    GH     githubapi.Client
    Notify notify.Notifier
    Repos  []string
    Prompt setup.PromptGetter
}

func BuildSummaryAgent(d Deps) (agent.Agent, error) {
    fetchers := make([]agent.Agent, 0, len(d.Repos))
    for _, repo := range d.Repos {
        fetchers = append(fetchers, newFetchAgent(repo, d.GH)) // code-agent, logic in summary.go
    }
    parallel, _ := parallelagent.New(parallelagent.Config{
        AgentConfig: agent.Config{Name: "fetch_all", SubAgents: fetchers},
    })

    summarize, _ := llmagent.New(llmagent.Config{
        Name: "summarizer", Model: d.LLM,
        Instruction: d.Prompt.Get("summary/summarize"),
        OutputKey:   "digest",
    })
    notifier := newNotifyAgent(d.Notify) // code-agent, logic in summary.go

    return sequentialagent.New(sequentialagent.Config{
        AgentConfig: agent.Config{Name: "summary_workflow", SubAgents: []agent.Agent{parallel, summarize, notifier}},
    })
}
```

Tests: `BuildSummaryAgent` with a fake `model.LLM` + fake `githubapi.Client` asserts
structure; `newFetchAgent`'s logic is tested against a stub GitHub server. The 80%+
coverage target falls out naturally because all hard logic lives in injectable, LLM-free
functions.

---

## 7. The three agents

**Root** (`root/`): receives the `ingest.Envelope`, routes by `Kind`. Initially a
deterministic dispatcher (code-agent with a `Run` that picks summary vs lintfixer); kept
as an *agent* (not a plain switch) so future ingest kinds (Jira/Confluence/human) and
LLM-based routing slot in without restructuring. Sub-agents are the summary and lint-fixer
workflows.

**Summary** (`summary/`): `Sequential[ Parallel[fetch_repo₁…fetch_repoₙ] → summarize(LLM) → notify ]`.
Repo list is `REPOS` env (comma-separated `owner/repo`), so N is dynamic — the parallel
fan-out is built from config at setup time. Fetchers use go-github `ListCommits` with
`Since: now-24h`. Summarizer is the reasoning LLM. Notify posts to Slack or Teams per
`NOTIFY_PROVIDER`.

**Lint-fixer** (`lintfixer/`) and **Coverage-fixer** (`covfixer/`): both are thin `Spec`s
over the shared `fixflow` engine. A deterministic **Sequencer** model drives a "fixer"
`LlmAgent` to emit a fixed `apply_fix → await_ci` sequence; `await_ci` is a long-running
(`IsLongRunning`) tool, so the run suspends and resumes on a GitHub `check_run` webhook.
See the dedicated section below — this is the complex one. Lint uses branch
`automation-agent/lint-fix`, label `automation-agent`, check `agent-lint-verify`; coverage
uses branch `automation-agent/test-coverage`, label `automation-agent-coverage`, check
`agent-coverage-verify`.

---

## 8. Suspend / resume design (CI feedback loop)

> **This section will be refined with the detailed notes from the prior discussion.**
> The structure below is the scaffold those notes drop into.

### The hard constraint: CI takes 20–40 minutes (often more with retries)

A fix can't be confirmed for 20–40 min (×3 iterations → up to ~2 h wall-clock), so the
workflow can't sit in a blocked goroutine. We don't run a local durable database either —
in-flight runs live **only in an in-memory parked-run registry**. GitHub holds the durable
PR artifacts:

- the **PR** exists on GitHub (number, branch, head SHA),
- the **check conclusion** exists on GitHub (the agent verify check),
- the **label** marks it as ours,
- the current findings are re-derivable by reading the check output / re-running the gate.

But GitHub is **not** consulted to recover in-flight state. When a fix applies and parks on
`await_ci`, the Driver records a `ParkedRun` (session id + call id + attempt count) in the
registry, keyed by PR, and arms a per-run `CI_TIMEOUT` timer. Consequences:

1. **No local DB, no file, no volume, no retention janitor, nothing to clean up manually.**
   Matches the "lightweight" goal.
2. **In-flight state is the registry — and it is non-durable.** A process restart loses the
   registry and **strands** any parked runs: those PRs are abandoned. This is an accepted
   trade-off; crash recovery is explicitly **out of scope**.
3. **Resume is webhook-driven, not a scan.** A GitHub `check_run` webhook looks the run up by
   PR key and resolves it; there is no periodic re-scan of labeled PRs. The per-run
   `CI_TIMEOUT` timer is the catch-all if CI never reports.
4. **Attempt count lives in the registry.** Each `ParkedRun` carries its `Attempts`; it is
   **not** derived from distinct agent-pushed GitHub SHAs.
5. **Idempotency via atomic resolve.** Exactly one of {webhook, timeout timer} resolves a run:
   `Resolve` atomically removes the registry entry, so late or duplicate deliveries (and a
   timer firing the same instant a webhook lands) find nothing and no-op — no dedupe table.
6. **Timeout is a real timer, not a derived timestamp.** Each parked run arms a `time.Timer`
   for `CI_TIMEOUT`; on fire, `onTimeout` claims the run and posts "needs human review" + PR
   link.

### Flow

```
lint payload ──▶ root ──▶ fixflow Driver (Sequencer-driven fixer)
   │
   │  attempt i:
   │   1. apply_fix(code): analyze + go-git clone/branch/edit/commit/push; go-github open/update PR
   │   2. await_ci       : LONG-RUNNING tool (IsLongRunning()=true) — returns "pending" now,
   │                       run SUSPENDS; Driver records a ParkedRun {session, call, attempts}
   │                       in the in-memory registry (keyed by PR) and arms a CI_TIMEOUT timer
   │
   ▼ (20–40+ min later)
/webhooks/github (check_run) ──▶ Driver.Resume: Resolve the parked run by PR key
                  ┌─ CI success ─▶ finish: post success summary (Slack/Teams) + PR link
                  ├─ CI failure & attempts < MAX_ITERATIONS ─▶ resume the run: apply_fix again WITH ci feedback
                  └─ CI failure & attempts == MAX_ITERATIONS ─▶ finish: "needs human review" + PR link
   │
   └─ (if CI never reports) CI_TIMEOUT timer ──▶ onTimeout: free the run, "needs human review" + PR link
```

### CI signal — a dedicated, label-triggered agent check (GitHub)

**Provider:** GitHub Actions / Checks API. Resume is driven by `check_run` (completed)
webhook events.

**Why a *dedicated* check, not the repo's existing lint check:** the existing PR lint check
is **diff-scoped** — it only flags problems on changed lines. That answers "did our change
introduce new lint?" but **not** "did we actually resolve the targeted findings?" (a finding
on a line we didn't touch, or a whole-file rule, would be missed). So we add our own check
that runs the *same* lint the kickoff payload came from and asserts: (a) every targeted
finding is gone, and (b) no new findings were introduced. Its single pass/fail is the
unambiguous resume signal.

**How it's triggered:** when the agent opens the PR it adds a label (default
`automation-agent`). The repo hosts one workflow:

```yaml
on:
  pull_request:
    types: [labeled, synchronize]   # labeled = first run; synchronize = each iteration's push
jobs:
  agent-lint-verify:
    if: contains(github.event.pull_request.labels.*.name, 'automation-agent')
    # runs full lint, compares against the targeted findings, reports the check conclusion
```

`synchronize` means the check re-runs automatically on every iteration's push, so we get a
fresh conclusion each loop with no extra orchestration. We listen only for *this check's
name* (`AGENT_CHECK_NAME`) completing; the repo's other checks are ignored.

### Two ingress endpoints

- `POST /webhooks/lint` — **kickoff.** Platform-agnostic lint JSON. May be posted by a
  scheduled GitHub Actions lint job (e.g. Mondays 09:00) or any other source. Starts the
  lint-fixer. (This replaces an internal Monday cron for lint — the schedule lives on the
  CI side.)
- `POST /webhooks/github` — **resume.** GitHub `check_run` events; verify
  `X-Hub-Signature-256` HMAC against `GITHUB_WEBHOOK_SECRET`.

### Correlation strategy (same-PR retry)

Retries push new commits to the **same** branch/PR (confirmed). The **PR key**
(`fullRepo#pr_number`) is the stable correlation key the registry is keyed on:

- Match an incoming `check_run` to a parked run by **PR key** (built from the event's repo +
  `pull_requests[].number`).
- `Resolve` atomically claims the run, so a late or duplicate delivery — or a timeout timer
  firing the same instant — finds nothing and no-ops.

We persist **nothing durably**. In-flight identity (session id, call id, attempt count) lives
in the **in-memory parked-run registry**; the only durable bit on GitHub is the PR itself
plus its label/check/SHA. A restart drops the registry and strands parked runs (accepted —
crash recovery is out of scope).

**Attempt count: tracked in the registry.** Each `ParkedRun` carries its `Attempts`; the
Driver increments it on each retry and compares against `MAX_ITERATIONS`. It is **not**
derived from GitHub SHAs. The give-up decision:

- **CI failed and attempts == `MAX_ITERATIONS` (3)** → post the failure summary
  (needs-human-review + PR link) to Slack/Teams and stop.
- **Per-run `CI_TIMEOUT` timer fires** (CI never reported) → same failure summary, timeout
  variant, via `onTimeout`.

Because the loop is bounded by `MAX_ITERATIONS` and the count lives with the run, it can
never run away.

### Two safety layers — webhook + per-run timeout (no scan, no ticker)

There is **no** reconcile loop and **no** periodic re-scan of labeled PRs. Resume rests on
two layers:

- **Webhook (fast path).** A GitHub `check_run` event resolves the parked run by PR key the
  moment CI finishes.
- **Per-run `CI_TIMEOUT` timer (catch-all).** When a run parks, it arms a `time.Timer` for
  `CI_TIMEOUT` (default 90m). If CI never reports — a missed or never-arriving webhook — the
  timer fires `onTimeout`, frees the run, and posts "needs human review" + PR link. Exactly
  one of {webhook, timer} wins, via the registry's atomic `Resolve`.
- **No retention/deletion step** — resolved runs are removed from the registry on resolve. A
  finished PR is merged or closed by the normal review workflow; that *is* the cleanup.

### ADK mechanics

- `await_ci` is implemented as a tool whose `IsLongRunning()` returns `true` — ADK's contract
  for "return a status now, finish later." The run suspends after dispatching it. A
  deterministic Sequencer model drives the fixer agent to emit a fixed `apply_fix → await_ci`
  sequence.
- Resume feeds the CI outcome back into the suspended run (by session id + call id) and drives
  the next `apply_fix → await_ci` step. adk-go has **no** durable engine, and we deliberately
  don't add one — `IsLongRunning` plus the in-memory parked-run registry is the suspend/resume
  mechanism.

### When a DB / shared registry enters the picture

A datastore (or shared registry) becomes worthwhile only when we want one of two things,
**neither of which is in scope today**:

- **Crash recovery.** The current registry is in-memory, so a restart strands parked runs. A
  durable store (small Postgres, or an engine like Temporal/River) would let runs survive a
  restart.
- **Multiple instances (HA / horizontal scale).** Two replicas can't share the in-memory
  registry; that needs a shared lock or work queue.

Either would slot behind the existing **registry + CI-handler** seam — the agent code doesn't
change. Single persistent instance with crash recovery out of scope: none of this is needed.

---

## 9. Prompts as markdown

All instructions live as `.md` files next to their agent (`prompts/*.md`), loaded via
`embed.FS` in `setup/prompt.go`:

```go
// per-package embed of its own prompts/ dir
//go:embed prompts/*.md
var prompts embed.FS
func Get(name string) string // "summarize" -> file contents
```

Matches the ADK-Go example convention of externalized instructions; keeps prompts
reviewable/diffable and lets non-code edits skip recompilation of logic.

---

## 10. ARCH tests, AGENTS.md, specs, Makefile

- **ARCH/** — `archtest`-style assertions:
  - `internal/agent/...` may import `internal/{githubapi,gitrepo,notify,config,ingest}`.
  - Tooling packages may **not** import `internal/agent/...`.
  - Nothing imports `cmd`.
  - Provider SDKs (ollama/gemini) may only be imported from `internal/agent/setup`.
  - A second test (`docs_test.go`) asserts every directory contains an `AGENTS.md`.
- **AGENTS.md everywhere** — one per directory + the root + `cmd/agent`. Inside each agent
  dir, a single *shared* `AGENTS.md` documents both `agents_setup.go` and `<name>.go`
  conventions.
- **specs/** — gitignored developer memory. `make spec name=add-jira-ingest kind=add`
  copies `.agents/templates/add.spec.md` → `specs/2026-…-add-jira-ingest.md`. Templates:
  **add / remove / change / migrate**, each with sections: Context, Motivation, Scope,
  Design, Test plan, Rollback, Checklist.
- **Makefile** targets: `build run test cover lint fmt vet arch tidy spec docs-check
  ollama-check ci`. `cover` fails under 80%; `arch` runs `go test ./ARCH/...`; `ci` chains
  `tidy lint vet arch test cover`.

---

## 11. Testing & coverage

- Unit tests for every logic function (`<name>.go`, tooling, adapters) → drives the 80%.
- `httptest` stubs for GitHub, Slack/Teams, Ollama.
- Build-agent tests use a `fakeLLM` implementing `model.LLM`.
- **No tests asserting LLM output content** (non-deterministic). Behavior validation is
  manual/eval. `make cover` enforces the 80% floor in CI.

---

## 12. Configuration

`.env.example` (typed in `internal/config`):

| Var | Purpose | Default |
|---|---|---|
| `LLM_PROVIDER` | `ollama` \| `gemini` | `ollama` |
| `OLLAMA_HOST` | Ollama base URL | `http://localhost:11434` |
| `OLLAMA_MODEL` | model tag | `gemma4:12b` |
| `GEMINI_MODEL` / Vertex creds | prod path | — |
| `REPOS` | comma-separated `owner/repo` | — |
| `GITHUB_TOKEN` | go-github auth | — |
| `NOTIFY_PROVIDER` | `slack` \| `teams` | `slack` |
| `SLACK_WEBHOOK_URL` / `TEAMS_WEBHOOK_URL` | notify targets | — |
| `PORT` | webhook server port | `8080` |
| `CRON_DAILY` / `CRON_WEEKLY` | schedules | `0 9 * * *` / `0 9 * * 1` |
| `MAX_ITERATIONS` | lint-fix loop cap | `3` |
| `CI_TIMEOUT` | per-run timer: how long a suspended fix run waits for its CI result before "needs review" | `90m` |
| `GITHUB_WEBHOOK_SECRET` | HMAC verify for `/webhooks/github` | — |
| `AGENT_PR_LABEL` | label that triggers the agent verify check | `automation-agent` |
| `AGENT_CHECK_NAME` | check name we resume on | `agent-lint-verify` |

---

## 13. Deployment

Target: a **persistent** GCP instance (always-on for cron + webhooks).

- **Cloud Run** with `min-instances=1` (keeps cron + webhook listener warm), or a **GCE VM**
  if we co-locate Ollama-on-GPU.
- **No persistent disk or database** — in-flight state lives only in the in-memory parked-run
  registry, so the service is lightweight. The trade-off: a restart/redeploy **strands** any
  in-flight fix runs (those PRs are abandoned; crash recovery is out of scope). Run it
  always-on (min-instances=1 or a VM) to receive webhooks and run the daily cron, and avoid
  redeploying while runs are parked.
- Secrets → **Secret Manager**, not plain `.env`.
- Model in prod → likely `LLM_PROVIDER=gemini` (Vertex) unless we provision a GPU VM for
  Ollama. Config flag, no code change.

---

## 14. Phased roadmap

Each phase is independently testable.

1. **Skeleton & standards** — repo tree, go.mod, Makefile, `.agents/` (standards +
   templates), ARCH tests, AGENTS.md, config, ingest envelope. *(no agents yet)*
2. **Model layer** — `setup`: Ollama adapter + Gemini factory + `BuildLLM` + prompt loader
   + runner. *(adapter tested vs stub Ollama)*
3. **Tooling** — `githubapi`, `gitrepo`, `notify`, `scheduler`, `webhook`.
   *(all unit-tested, agent-free)*
4. **Root + Summary** — end-to-end summary workflow on a real repo via local Gemma →
   Slack/Teams.
5. **Lint-fixer** — the suspend/resume workflow, incorporating the detailed notes.
6. **Deployment** — Cloud Run (min-instances=1) or GCE; decide Ollama-on-GPU vs Gemini.

---

## 15. Open questions

1. **Persistence:** ✅ no durable store — in-flight runs live in an **in-memory parked-run
   registry**; GitHub holds the durable PR artifacts but is not consulted to recover in-flight
   state. Non-durable: a restart strands parked runs (crash recovery is out of scope). See §8.
2. **Notify:** build the `Notifier` interface + both Slack and Teams impls; choice is one
   env var. Teams targets the new **Workflows/Adaptive Card** format (O365 connectors
   deprecating). ✅ assumed.
3. **Root routing:** start deterministic; add LLM routing later. ✅ assumed.
4. **Lint-fixer:** hold detailed suspend/resume impl until the prior notes are shared.
5. **CI provider:** ✅ GitHub Actions / Checks API. Resume listens for `check_run`
   (completed) on a dedicated, **label-triggered** agent verification check (`synchronize`
   re-runs it each iteration). See §8.
6. **Same-PR vs new-PR on retry:** ✅ same PR/branch — iterations push new commits to the
   existing branch. Correlation key is therefore `pr_number`, with `current_head_sha` as a
   freshness guard.

---

## 16. Verified ADK-Go API reference

Import root is `google.golang.org/adk/...` (repo `github.com/google/adk-go`).

```go
// LLM agent
llmagent.New(llmagent.Config{
    Name, Description string
    Model       model.LLM
    Instruction string            // supports {var} placeholders; InstructionProvider for dynamic
    Tools       []tool.Tool
    SubAgents   []agent.Agent
    OutputKey   string            // writes result into session state
    // + Before/After model|tool|agent callbacks, Toolsets, In/OutputSchema, IncludeContents
})

// Custom / code agent
agent.New(agent.Config{
    Name, Description string
    SubAgents []agent.Agent
    Run func(InvocationContext) iter.Seq2[*session.Event, error]
})

// Workflow agents (package google.golang.org/adk/agent/workflowagents/...)
loopagent.New(loopagent.Config{ MaxIterations: 3, AgentConfig: agent.Config{...} })
sequentialagent.New(sequentialagent.Config{ AgentConfig: agent.Config{...} })   // shape to confirm
parallelagent.New(parallelagent.Config{ AgentConfig: agent.Config{...} })       // shape to confirm

// Model interface (package google.golang.org/adk/model)
type LLM interface {
    Name() string
    GenerateContent(ctx context.Context, req *LLMRequest, stream bool) iter.Seq2[*LLMResponse, error]
}

// Long-running tool hook (package google.golang.org/adk/tool)
type Tool interface { /* ... */ IsLongRunning() bool }
// ToolContext.RequestConfirmation(hint, payload) exists for human-in-the-loop pauses.
```

Notes:
- `loopagent` shape is verified from the official example
  (`examples/workflowagents/loop/main.go`). Sequential/parallel are assumed to share the
  embedded-`agent.Config` shape — to confirm against their example dirs during Phase 1.
- adk-go has **no** durable workflow engine; `IsLongRunning` (the long-running `await_ci`
  tool) + the in-memory parked-run registry is the suspend/resume mechanism.
