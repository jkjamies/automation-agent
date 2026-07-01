# Automation Agent — Architecture & Build Plan

> This is the living design doc for the system. The CI feedback loop runs on
> **durable sessions** (§8): one `SESSION_BACKEND` env selects an in-memory (default),
> sqlite (durable local), or firestore (cloud) backend, so a parked run survives a process
> restart — the change that unlocks Cloud Run scale-to-zero. Per-port parity is tracked
> per-PR (see [`language-parity.md`](language-parity.md)).

A single long-running Go service that ingests events from many sources, routes every
ingest through a **Root Agent**, and runs three workflow agents: a **Summary** workflow
(daily commit digest), a **Lint-fixer** workflow (autonomous lint remediation
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
14. [System composition](#14-system-composition)
15. [Open questions](#15-open-questions)
16. [Verified ADK-Go API reference](#16-verified-adk-go-api-reference)

---

## 1. Goals

1. **Ingest events** from many possible sources. Today: a **daily** Cloud Scheduler
   trigger. Tomorrow: GitHub / Jira / Confluence / human-triggered. Every
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
                       ┌──────────────────── ingest sources ────────────────────┐
   Cloud Scheduler ─┐   │ POST /webhooks/{lint,coverage}   POST /webhooks/github  │
   (daily) ─────────┼──▶│ managed API gateway ──► HTTP server                     │
   GitHub App ──────┘   │ (github: check_run resume + pull_request review)        │
                        └────────────────────────────┬───────────────────────────┘
                                                     ▼
                                  ingest.Envelope (normalized; routed by Kind)
                                                     ▼
                                            ┌───────────────┐
                                            │  ROOT AGENT   │  (dispatcher)
                                            └───────┬───────┘
        ┌──────────────────┬──────────────────┬─────┴──────────────┐
        ▼                  ▼                   ▼                    ▼
 ┌────────────┐   ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐
 │  SUMMARY   │   │   LINT-FIXER     │  │  COVERAGE-FIXER  │  │     REVIEWER     │
 │ KindCron   │   │ KindLint (+CI)   │  │ KindCoverage(+CI)│  │   KindReview     │
 │ Sequential:│   │  (fixflow Spec)  │  │  (fixflow Spec)  │  │ intake → filter  │
 │ Parallel[  │   │  apply_fix(PR)   │  │  apply_fix(PR)   │  │ → size-gate →    │
 │  fetch×N]  │   │  → await_ci      │  │  → await_ci      │  │ Parallel[lenses] │
 │ →summarize │   │   (suspend)      │  │   (suspend)      │  │ → glue →         │
 │ →notify    │   │  → resume        │  │  → resume        │  │ scorecard →      │
 │            │   │  → notify        │  │  → notify        │  │ publish (advisory)│
 └─────┬──────┘   └────────┬─────────┘  └────────┬─────────┘  └────────┬─────────┘
       ▼                   ▼                     ▼                     ▼
  Slack / Teams       Slack / Teams         Slack / Teams      PR review + agent-review check
```

Lint-fixer and Coverage-fixer share the generic `fixflow` engine; each is a thin `Spec`
(branch/label/check + triage/analyze) over it. The **Reviewer** is a separate workflow,
triggered by the native `pull_request` event (`KindReview`); unlike the fixers it does **not**
suspend/resume — it runs one-shot **in-request** and posts an advisory, comment-only review
(it opens no PR/branch). See §8 and [`webhooks.md`](webhooks.md).

Tooling (`gitrepo`, `githubapi`, `webhook`, `notify`) is **deterministic
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
| HTTP | `net/http` (`ServeMux`, Go 1.22 method routing) | stdlib is enough; chi only if we outgrow it |
| GitHub API | `github.com/google/go-github` | list commits, create PR |
| Git working tree | `github.com/go-git/go-git/v5` | clone/branch/commit/push (pure Go) |
| Arch tests | `github.com/matthewmcnew/archtest` or hand-rolled `go/packages` | import-boundary assertions |
| Lint | `golangci-lint` (incl. `depguard`) | quality gate |
| Suspend/resume state | adk `session.Service` + `setup.ParkStore` (both `SESSION_BACKEND`-switched) | the parked fix loop's state — the ADK suspend/resume event history *and* the park record (PR key → session/call id, attempt count, serialized run params) — is held by two provider-switched stores: `memory` (default, in-process), `sqlite` (local file), or `firestore` (cloud). A per-run `CI_TIMEOUT` timer fast-paths each wait; the `ParkStore` sweep is the durable catch-all. With a durable backend a process restart **resumes** parked runs cleanly (the change that unlocks Cloud Run scale-to-zero); `memory` keeps the old non-durable behavior. Both deps are confined to `internal/agent/setup` (ARCH-enforced) |
| Durable-session SDKs | `github.com/glebarez/sqlite`, `gorm.io/gorm`, `cloud.google.com/go/firestore` | back the sqlite + firestore session/park stores; **setup-only** (ARCH-enforced) |

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
├── DEPLOYMENT.md                  # deployment status + setup checklist
├── .gitignore                     # ignores: /specs/* (keep .gitkeep), .env, *.db, build/test artifacts
├── .env.example
│
├── .agents/                       # open-standard knowledge for agents (port-agnostic)
│   ├── AGENTS.md                  # documents this whole .agents/ tree (subdirs have no own AGENTS.md)
│   ├── skills/                    # reusable task recipes (e.g. add-workflow-agent.md)
│   ├── standards/                 # the rules + canonical design/reference docs
│   │   ├── architecture-design.md # THE authoritative design (this document)
│   │   ├── architecture.md        # the import boundaries ARCH enforces
│   │   ├── language-parity.md     # the cross-language 1:1 contract
│   │   ├── ci-integration.md      # how CI sends lint/coverage reports
│   │   ├── deployment.md          # local + cloud deployment
│   │   ├── local-development.md   # running the agent locally
│   │   ├── go-style.md
│   │   ├── testing.md             # 80% rule, no-LLM-assert rule
│   │   ├── agent-build-pattern.md # the setup-vs-logic split
│   │   └── documentation.md       # the docs-move-with-the-code rule
│   └── templates/                 # spec templates
│       ├── add.spec.md
│       ├── remove.spec.md
│       ├── change.spec.md
│       └── migrate.spec.md
│
├── specs/                         # local dev/review docs (`/specs/*` gitignored; `.gitkeep` kept)
│   └── .gitkeep
│
├── go/                            # the Go port (source of truth); siblings: python/ kotlin/ javascript/
│   ├── AGENTS.md
│   ├── README.md / Makefile / go.mod / go.sum / Dockerfile
│   ├── .golangci.yml
│   │
│   ├── cmd/agent/
│   │   ├── main.go                # wire config→tooling→runner→http; block
│   │   └── AGENTS.md
│   │
│   ├── ARCH/                      # architecture tests (its own package)
│   │   ├── arch_test.go           # import-boundary rules
│   │   ├── docs_test.go           # assert AGENTS.md presence per dir
│   │   └── AGENTS.md
│   │
│   └── internal/
│       ├── AGENTS.md
│       ├── config/                # env → typed Config; one source of truth
│       │   ├── config.go
│       │   └── AGENTS.md
│       ├── ingest/                # the normalized Envelope + Kind enum (cron/ci/github/jira…)
│       │   ├── envelope.go
│       │   └── AGENTS.md
│       ├── agent/
│       │   ├── AGENTS.md          # SHARED agent doc: explains setup.go vs <name>.go convention
│       │   ├── setup/             # common agent utilities
│       │   │   ├── llm.go         # BuildLLM (provider switch)
│       │   │   ├── ollama.go      # model.LLM adapter
│       │   │   ├── gemini.go      # gemini factory
│       │   │   ├── prompt.go      # embed.FS prompt loader -> GetPrompt("summary/summarize")
│       │   │   ├── events.go      # helpers to emit/parse session.Event + text
│       │   │   ├── runner.go      # build adk Runner + (ephemeral) session service
│       │   │   ├── session.go     # NewSessionService: SESSION_BACKEND switch (memory|sqlite|firestore)
│       │   │   ├── session_firestore.go # custom firestore-backed session.Service (cloud)
│       │   │   ├── parkstore.go   # ParkStore interface + memory impl (the park record)
│       │   │   ├── parkstore_sqlite.go    # gorm/sqlite ParkStore (durable local)
│       │   │   ├── parkstore_firestore.go # firestore ParkStore (cloud)
│       │   │   ├── longrun.go     # LongRunDriver: ADK suspend/resume over a session.Service
│       │   │   └── AGENTS.md
│       │   ├── root/
│       │   │   ├── agents_setup.go    # BuildRootDispatcher(deps) -> *Dispatcher
│       │   │   ├── root.go            # dispatch logic (Run func / callbacks), testable
│       │   │   ├── prompts/root.md
│       │   │   └── AGENTS.md
│       │   ├── summary/
│       │   │   ├── agents_setup.go    # BuildSummaryAgent(deps) -> Sequential[Parallel[fetch×N]→sum→notify]
│       │   │   ├── summary.go         # fetch code-agent + summarize logic
│       │   │   ├── prompts/summarize.md
│       │   │   ├── tasks/             # (optional) per-step helpers
│       │   │   └── AGENTS.md
│       │   ├── lintfixer/             # the lint Spec of the fixflow engine
│       │   │   ├── lint.go            # builds the lint Spec (branch/label/check + triage/analyze)
│       │   │   ├── prompts/
│       │   │   └── AGENTS.md
│       │   ├── covfixer/              # the coverage Spec of the fixflow engine
│       │   │   ├── coverage.go        # builds the coverage Spec
│       │   │   ├── prompts/
│       │   │   └── AGENTS.md
│       │   └── fixflow/               # generic fix engine shared by lint + coverage
│       │       ├── engine.go          # Spec-driven engine (triage→analyze→commit→PR)
│       │       ├── driver.go          # suspend/resume Driver (Kickoff/Resume/onTimeout/SweepTimeouts) over a ParkStore
│       │       ├── summary.go         # status-aware terminal summaries (success/exhausted/timeout)
│       │       ├── applyfix.go        # one fix attempt: checkout/edit/commit/push/PR
│       │       ├── analyze.go         # analyze step
│       │       ├── explore.go         # repo exploration helper
│       │       ├── tools.go           # apply_fix + long-running await_ci tools
│       │       ├── files.go
│       │       ├── util.go
│       │       ├── envelope.go
│       │       └── AGENTS.md
│       ├── githubapi/                 # go-github: ListCommits, CreatePR, check status
│       │   ├── client.go
│       │   └── AGENTS.md
│       ├── gitrepo/                   # go-git: Clone, Branch, Commit, Push
│       │   ├── repo.go
│       │   └── AGENTS.md
│       ├── webhook/                   # http.Server + handlers (daily/ci/ingest)
│       │   ├── server.go
│       │   ├── handlers.go
│       │   └── AGENTS.md
│       └── notify/                    # Slack/Teams behind one interface
│           ├── notify.go              # Notifier interface
│           ├── slack.go
│           ├── teams.go               # plan for Workflows/Adaptive Card (O365 connectors deprecating)
│           └── AGENTS.md
│
├── python/                        # the Python port (mirrors go/ topology)
├── kotlin/                        # the Kotlin port (mirrors go/ topology)
└── javascript/                    # the TypeScript port (mirrors go/ topology)
```

Suspend/resume state is split across two `internal/agent/setup`-owned stores, both selected
by one `SESSION_BACKEND` env (`memory`|`sqlite`|`firestore`): the ADK `session.Service`
(suspend/resume event history) and the `setup.ParkStore` (the park record — `prKey→sessionID`,
attempts, serialized run params). The `fixflow` Driver holds a `ParkStore`, not an in-process
map. Resume is webhook-driven (fast path), with a per-run `CI_TIMEOUT` timer **and** the
durable `ParkStore` sweep (driven by Cloud Scheduler via `/internal/sweep`) as catch-alls.
There is no PR-scan ticker over labeled PRs. With a durable backend a process restart resumes
parked runs; `memory` (default) keeps the old non-durable behavior.

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
See the dedicated section below — this is the complex one. Both workflows share the single
`AGENT_PR_LABEL` (`automation-agent`, write-only) and are told apart by branch + verify
check: lint uses branch `automation-agent/lint-fix`, check `agent-lint-verify`; coverage
uses branch `automation-agent/test-coverage`, check `agent-coverage-verify`.

---

## 8. Suspend / resume design (CI feedback loop)

> **Durable sessions.** One `SESSION_BACKEND` env (`memory`|`sqlite`|`firestore`) selects
> two provider-switched stores; `memory` is the zero-dependency default, `firestore` is the
> prod path. Per-port parity is tracked per-PR (see [`language-parity.md`](language-parity.md)).

> **Scope: the fixers only.** This section describes the lint/coverage fixers, which wait on
> CI and so suspend/resume. The **Reviewer** does **not** park: it has no CI to wait on, so it
> runs one-shot **in-request** (its multi-minute LLM compute rides the execution transport;
> see §13) and posts an advisory review. It keeps no per-run session/park record — re-review
> idempotency comes from reconciling against the PR's own comments (GitHub-as-store).

### The hard constraint: CI takes 20–40 minutes (often more with retries)

A fix can't be confirmed for 20–40 min (×3 iterations → up to ~2 h wall-clock), so the
workflow can't sit in a blocked goroutine — the run **suspends** and **resumes** on the CI
webhook. Where that suspended state lives is a config choice, not a hardcoded "in-memory only":

**One env, two provider-switched stores (both confined to `internal/agent/setup`):**

- a durable ADK **`session.Service`** — the suspend/resume *event history* the agent needs to
  continue a parked run, and
- a custom **`setup.ParkStore`** — the *park record*: `prKey→sessionID`, attempt count, the
  parked long-running call id, and the run's serialized params (so a retry — or a restart —
  can reconstruct exactly what to apply). `Params` is an opaque blob the store never
  interprets, which keeps it free of fixflow types and lets one interface back all three
  backends.

`SESSION_BACKEND` picks the pair:

| `SESSION_BACKEND` | session.Service | ParkStore | Durable across restart? | Use |
|---|---|---|---|---|
| `memory` (default) | in-process | in-process map | **no** | tests, ephemeral local runs |
| `sqlite` | adk `session/database` (file) | gorm/sqlite (same file) | **yes** | durable local runs |
| `firestore` | custom firestore `session.Service` | firestore | **yes** | cloud (scale-to-zero) |

GitHub still holds the durable PR artifacts (PR number/branch/head SHA, the check conclusion,
the `automation-agent` label) and the findings remain re-derivable from the check output — but
GitHub is **not** scanned to recover in-flight state. Instead, when a fix applies and parks on
`await_ci`, the Driver writes a park record to the `ParkStore` (keyed by sessionID, indexed by
PR key) and arms a per-run `CI_TIMEOUT` timer. Consequences:

1. **The `memory` default keeps it lightweight** — no DB, no file, no volume, nothing to clean
   up — exactly the old behavior, for tests and throwaway local runs.
2. **A durable backend survives a restart.** With `sqlite` (local) or `firestore` (prod) the
   park record and session history outlive the process, so a restart **resumes** in-flight
   runs cleanly rather than stranding them. **This is what unlocks Cloud Run scale-to-zero**:
   the instance can be torn down between events and rehydrate the parked run when CI reports.
3. **Session IDs are UUIDs.** A shared/durable store is accessed across restarts (and
   potentially instances), so a process-local counter would collide or overwrite persisted
   runs — kickoff mints a `uuid.NewString()`.
4. **Resume is webhook-driven, not a scan.** A GitHub `check_run` webhook looks the run up by
   PR key and resolves it; there is no periodic re-scan of labeled PRs.
5. **Attempt count lives in the park record.** Each record carries its `Attempts`; it is
   **not** derived from distinct agent-pushed GitHub SHAs.
6. **Idempotency via an atomic single-winner claim.** `ResolveByPRKey` (and `Sweep`) clears
   the PR index atomically in every backend — a mutex (memory), a conditional `UPDATE … WHERE
   pr_key = ?` CAS (sqlite), or a transaction (firestore) — so of N concurrent claimers
   (a late/duplicate webhook, or a timer racing a webhook) exactly one wins and the rest no-op.
   No dedupe table. The per-run record is *retained* on resolve (a retry still needs its
   params); terminal `clear` is what deletes it.
7. **Eager cleanup so durable backends don't leak.** Terminal `clear` deletes the park record
   **and** calls `LongRunDriver.DeleteSession`, removing the ADK session too — otherwise a
   durable backend would accumulate completed sessions.
8. **Two timeout layers.** A per-run `time.Timer` (`CI_TIMEOUT`, default 90m) is the fast,
   in-process catch-all; it is lost on restart, so the durable `ParkStore.Sweep` (driven by
   Cloud Scheduler via `/internal/sweep`) is the restart-safe catch-all. The atomic claim
   makes a webhook racing either timer safe.

### Flow

```
lint payload ──▶ root ──▶ fixflow Driver (Sequencer-driven fixer, holds a ParkStore)
   │
   │  Kickoff: mint sessionID (UUID); Put run params in the ParkStore
   │  attempt i:
   │   1. apply_fix(code): load run params from ParkStore by sessionID (never model-supplied);
   │                       analyze + go-git clone/branch/edit/commit/push; go-github open/update PR
   │   2. await_ci       : LONG-RUNNING tool (IsLongRunning()=true) — returns "pending" now,
   │                       run SUSPENDS; Driver parks the record {sessionID, prKey, callID,
   │                       attempts, params} in the ParkStore and arms a CI_TIMEOUT timer.
   │                       The session.Service holds the suspend/resume event history.
   │                       (sqlite/firestore: both persist → a restart can resume.)
   │
   ▼ (20–40+ min later)
/webhooks/github (check_run) ──▶ Driver.Resume: ResolveByPRKey atomically claims the run
                  ┌─ CI success ─▶ finish: post success summary (Slack/Teams) + PR link; clear
                  ├─ CI failure & attempts < MAX_ITERATIONS ─▶ resume the run: apply_fix again WITH ci feedback
                  └─ CI failure & attempts == MAX_ITERATIONS ─▶ finish: "needs human review" + PR link; clear
   │
   ├─ (CI never reports, warm)    CI_TIMEOUT timer ─▶ onTimeout: claim, notify "needs review", clear
   └─ (CI never reports, restarted) POST /internal/sweep ─▶ ParkStore.Sweep: claim stale, notify, clear

   clear = ParkStore.Delete + LongRunDriver.DeleteSession (no leaked sessions on durable backends)
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

**How it's triggered:** when the agent opens the PR it adds the `AGENT_PR_LABEL` label
(default `automation-agent`) and pushes to a per-workflow branch. The repo hosts one
workflow per fixer, each gated on its **branch** (the shared label can't tell lint from
coverage apart):

```yaml
on:
  pull_request:
    types: [labeled, synchronize]   # labeled = first run; synchronize = each iteration's push
jobs:
  agent-lint-verify:
    if: github.event.pull_request.head.ref == 'automation-agent/lint-fix'
    # runs full lint, compares against the targeted findings, reports the check conclusion
```

`synchronize` means the check re-runs automatically on every iteration's push, so we get a
fresh conclusion each loop with no extra orchestration. We listen only for *this workflow's
verify check* (e.g. `agent-lint-verify`, hardcoded per workflow) completing; the repo's
other checks are ignored.

### Ingress endpoints

**Webhook ingress (HMAC, `GITHUB_WEBHOOK_SECRET`):**

- `POST /webhooks/lint` / `POST /webhooks/coverage` — **kickoff.** Platform-agnostic lint /
  coverage JSON. May be posted by a scheduled GitHub Actions job or any other source. Starts
  the fixer. (This replaces an internal cron for the kickoff — the schedule lives CI-side.)
- `POST /webhooks/github` — **resume.** GitHub `check_run` events.

**Cloud Scheduler ingress (Bearer token, `INTERNAL_TOKEN`; disabled → 404 when unset):**

- `POST /internal/cron/daily` — externalizes the commit-digest schedule so it lives GCP-side
  and the instance can scale to zero between fires. Cloud Scheduler is the only trigger; the
  service runs no in-process cron.
- `POST /internal/sweep` — the **durable timeout catch-all**: drives `ParkStore.Sweep` /
  `Engine.SweepTimeouts`, resolving every parked run whose CI never reported within
  `CI_TIMEOUT`. This is the restart-safe counterpart to the in-process per-run timer.

### Correlation strategy (same-PR retry)

Retries push new commits to the **same** branch/PR (confirmed). The **PR key**
(`fullRepo#pr_number`) is the per-park resume index the `ParkStore` maintains over the
sessionID-keyed record:

- Match an incoming `check_run` to a parked run by **PR key** (built from the event's repo +
  `pull_requests[].number`).
- `ResolveByPRKey` atomically claims the run (clears the PR index), so a late or duplicate
  delivery — or a timeout timer firing the same instant — finds nothing and no-ops. The
  per-run record is retained so a retry can still read its params; terminal `clear` deletes it.

What persists depends on `SESSION_BACKEND`: with `memory` (default) nothing persists across a
restart (old behavior); with `sqlite`/`firestore` the park record and the ADK session history
both persist, so a restart resumes the run. Session identity is a **UUID** (a process-local
counter would collide once the store is shared/durable). The PR itself plus its label/check/SHA
remain the durable artifacts on GitHub.

**Attempt count: tracked in the park record.** Each record carries its `Attempts`; the
Driver increments it on each retry and compares against `MAX_ITERATIONS`. It is **not**
derived from GitHub SHAs. The give-up decision:

- **CI failed and attempts == `MAX_ITERATIONS` (3)** → post the failure summary
  (needs-human-review + PR link) to Slack/Teams and stop.
- **Per-run `CI_TIMEOUT` timer fires** (CI never reported) → same failure summary, timeout
  variant, via `onTimeout`.

Because the loop is bounded by `MAX_ITERATIONS` and the count lives with the run, it can
never run away.

### Safety layers — webhook + per-run timer + durable sweep (no PR-scan ticker)

There is **no** reconcile loop and **no** periodic re-scan of labeled PRs. Resume rests on
three layers, all funnelling through the `ParkStore`'s atomic single-winner claim:

- **Webhook (fast path).** A GitHub `check_run` event resolves the parked run by PR key the
  moment CI finishes.
- **Per-run `CI_TIMEOUT` timer (warm catch-all).** When a run parks it arms a `time.Timer`
  for `CI_TIMEOUT` (default 90m). If CI never reports, `onTimeout` claims the run and posts
  "needs human review" + PR link. The timer is in-process, so a restart loses it — hence:
- **`ParkStore.Sweep` (durable catch-all).** Cloud Scheduler POSTs `/internal/sweep`, which
  claims every parked record whose `ParkedAt` precedes `now − CI_TIMEOUT` and resolves it the
  same way. This is the restart-safe replacement for the lost timer. Exactly one of {webhook,
  timer, sweep} wins, via the store's atomic claim (mutex / sqlite CAS / firestore txn).
- **Eager terminal cleanup.** On resolve the Driver `clear`s the run — `ParkStore.Delete` +
  `LongRunDriver.DeleteSession` — so a durable backend does not leak completed sessions. (A
  finished PR is still merged/closed by the normal review workflow.) A *separate* orphan-session
  GC for sessions that crash between create-and-park is not yet implemented — see
  [`DEPLOYMENT.md`](../../DEPLOYMENT.md).

### ADK mechanics

- `await_ci` is implemented as a tool whose `IsLongRunning()` returns `true` — ADK's contract
  for "return a status now, finish later." The run suspends after dispatching it. A
  deterministic Sequencer model drives the fixer agent to emit a fixed `apply_fix → await_ci`
  sequence.
- Resume feeds the CI outcome back into the suspended run (by session id + call id) and drives
  the next `apply_fix → await_ci` step. adk-go has **no** durable *workflow* engine; we supply
  durability at the **session** layer instead — `IsLongRunning` over a `SESSION_BACKEND`-selected
  `session.Service`, plus the `ParkStore` for the run record, is the suspend/resume mechanism.
  The `LongRunDriver` (`setup/longrun.go`) is the generic plumbing: `Start` runs to a park,
  `Resume` feeds a result back, `DeleteSession` cleans up; it carries no fixflow policy.

### Status-aware terminal summaries

A finished run posts a status-aware summary (`fixflow/summary.go`) framed by outcome —
**success**, **max-iter exhausted**, or **timeout**. The per-attempt work product lives only on
the PR (commits + diff), never in the session, so the summary enriches the notification by
calling `githubapi.Compare` (base…branch: commit count + changed files) and pulling the original
findings + attempt count from the park record. The compare is best-effort: on error the summary
still reports attempts and findings.

### Crash recovery and multiple instances

What used to be "out of scope" is now the **default-off** behavior, switchable by config:

- **Crash recovery.** With `SESSION_BACKEND=sqlite` (local) or `firestore` (cloud) the park
  record and ADK session history persist, so a restart resumes parked runs — no Postgres or
  Temporal/River needed. `memory` (default) keeps the non-durable behavior for tests/throwaway
  runs.
- **Multiple instances (HA / horizontal scale).** `firestore` is a shared store and every claim
  (`ResolveByPRKey`/`Sweep`) is a single-winner transaction, so replicas can in principle share
  it safely; running multiple instances is not exercised yet, but the seam is there.

This all sits behind the **`session.Service` + `ParkStore`** interfaces in `internal/agent/setup`
— the agent code is identical across backends.

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
- **Docs + diagrams move with the code (a change is not done until they do).** `docs_test`
  only checks that an `AGENTS.md` **exists**, not that it is current — freshness is on the
  author. When an agent, ingest `Kind`, ingress route, or tooling package changes, sweep every
  place that describes or draws it (the package `AGENTS.md`; the root + `agent/root` diagrams;
  the §2/§13 and `deployment.md` topology diagrams; `.env.example` + the §12 config table) in
  the same change, mirrored across all ports. Full checklist:
  [`documentation.md`](documentation.md).
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
| `SESSION_BACKEND` | where the durable suspend/resume session **and** park record live: `memory` (default, in-process) \| `sqlite` (durable local) \| `firestore` (cloud) | `memory` |
| `SQLITE_DSN` | sqlite data source (used when `=sqlite`) | `file:automation-agent.db?_pragma=busy_timeout(5000)` |
| `FIRESTORE_PROJECT` | GCP project (used when `=firestore`); blank = detect from ADC / `GOOGLE_CLOUD_PROJECT` | — |
| `FIRESTORE_COLLECTION` | collection-name prefix (`_sessions`, `_app_state`, `_user_state`, `_parked_runs`) | `automation_agent` |
| `REPOS` | comma-separated `owner/repo`; also the kickoff allowlist — when non-empty, the fix-loop only acts on listed repos (empty = no restriction in PAT mode; **required** in App mode — empty is rejected) | — |
| `GITHUB_TOKEN` | PAT auth (PR create/label/compare + `https` git transport x-access-token); the **local-dev fallback**, used when the `GITHUB_APP_*` vars are unset | — |
| `GITHUB_APP_ID` | numeric GitHub App ID; presence (with a key + installation id) selects **App mode** (production auth — short-lived, repo-scoped installation tokens) | — |
| `GITHUB_APP_INSTALLATION_ID` | pinned installation for this deployment's single org; **required in App mode** | — |
| `GITHUB_APP_PRIVATE_KEY_PATH` | path to the App private-key `.pem` (local dev); exactly one of key/path required in App mode | — |
| `GITHUB_APP_PRIVATE_KEY` | literal PEM contents of the App private key (cloud / Secret Manager); a flattened `\n` is auto-restored | — |
| `GIT_TRANSPORT` | git clone/push transport: `https` (token / GitHub App) \| `ssh` (local dev — ssh-agent/keys). SSH covers only the git transport; the REST API still needs `GITHUB_TOKEN`/`gh` login (an `ssh` run without one warns at startup) | `https` |
| `GIT_SSH_KEY` | `GIT_TRANSPORT=ssh`: explicit private-key path; blank = ssh-agent then `~/.ssh/id_ed25519\|id_rsa\|id_ecdsa` | — |
| `NOTIFY_PROVIDER` | `slack` \| `teams` | `slack` |
| `SLACK_WEBHOOK_URL` / `TEAMS_WEBHOOK_URL` | notify targets | — |
| `PORT` | webhook server port | `8080` |
| `MAX_ITERATIONS` | lint-fix loop cap | `3` |
| `CI_TIMEOUT` | how long a suspended fix run waits for its CI result before the timer/sweep frees it ("needs review") | `90m` |
| `GITHUB_WEBHOOK_SECRET` | HMAC verify for `/webhooks/*` | — |
| `INTERNAL_TOKEN` | Bearer token for `/internal/*` (Cloud Scheduler cron + sweep, **and** the Cloud Tasks `/internal/dispatch` worker); empty disables them (404) | — |
| `TASKS_BACKEND` | execution transport for webhook-triggered work: `inprocess` (in-process background worker pool — local/default; not durable) \| `cloudtasks` (enqueue → Cloud Tasks HTTP-target → `POST /internal/dispatch`, run **in-request** so Cloud Run keeps CPU allocated) | `inprocess` |
| `TASKS_PROJECT` | GCP project owning the Cloud Tasks queue (`cloudtasks` only); blank = `GOOGLE_CLOUD_PROJECT` | `GOOGLE_CLOUD_PROJECT` |
| `TASKS_LOCATION` | Cloud Tasks queue region, e.g. `us-central1` (**required** for `cloudtasks`) | — |
| `TASKS_QUEUE` | Cloud Tasks queue name (**required** for `cloudtasks`) | — |
| `DISPATCH_URL` | absolute `https://` URL the queue POSTs to; must end in `/internal/dispatch` (**required** for `cloudtasks`) | — |
| `TASKS_DISPATCH_DEADLINE` | explicit per-task dispatch deadline; range `15s`..`30m` (Cloud Tasks HTTP-target max; unset queue default is only 10m); `cloudtasks` only | `30m` |
| `AGENT_PR_LABEL` | label applied to every agent PR on creation (write-only — PR lookup is by branch) | `automation-agent` |
| `REVIEW_ENABLED` | PR code-review kill switch: `false` accepts/acknowledges `pull_request` events but runs no reviewer work; `true` runs the full reviewer (intake → category lenses → glue → scorecard → publish) | `false` |
| `REVIEW_SKIP_DRAFTS` | skip draft PRs unless the triggering action is `ready_for_review` | `true` |
| `REVIEW_EXCLUDE_GLOBS` | comma-separated globs dropped before sizing/review (generated, vendored, lockfiles, minified, binaries); blank uses the built-in default set | (built-in set) |
| `REVIEW_MAX_FILES` | size-gate cap on changed files (measured on the **filtered** diff); over it the PR is denied (review-or-deny) | `50` |
| `REVIEW_MAX_DIFF_BYTES` | size-gate cap on total filtered patch bytes; over it the PR is denied | `262144` (256 KiB) |
| `REVIEW_MIN_CONFIDENCE` | drop findings below this confidence before scoring (phase-1 verify gate); must be in `[0,1]`, non-positive keeps all | `0.6` |
| `REVIEW_DEBOUNCE` | coalesce rapid pushes: a `synchronize` review is enqueued under a per-PR-per-window Cloud Tasks name with this delay, so a burst collapses to one review of the latest SHA (Cloud Tasks backend only) | `30s` |
| `REVIEW_STANDARDS` | standards-aware review: discover the **reviewed** repo's own convention docs, distill them, and steer the lenses off them; `false` reviews generically | `true` |
| `REVIEW_STANDARDS_GLOBS` | comma-separated discovery globs matched against the reviewed repo's tree (`AGENTS.md`, `.cursor/rules/**`, `CLAUDE.md`, …); blank uses the built-in default set | (built-in set) |
| `REVIEW_STANDARDS_MAX_BYTES` | cap on total convention-doc bytes fed to the distiller; must be positive | `262144` (256 KiB) |
| `REVIEW_UNCITED_MODE` | how a conformance finding citing no real repo rule is handled: `nitpick` (demote) or `drop` | `nitpick` |
| `OTEL_TRACES_EXPORTER` | tracing sink / kill switch: `none` (no-op — merging changes nothing) · `console` (stdout) · `otlp` (any OTLP-native backend or a Collector) · `gcp` (Cloud Trace via ADC). The app names no vendor | `none` |
| `OTEL_SERVICE_NAME` | resource `service.name` on every span | `automation-agent` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP/HTTP target URL; **required** when exporter=`otlp` (an otlp exporter with no target is rejected) | — |
| `OTEL_EXPORTER_OTLP_HEADERS` | OTLP auth headers (comma-separated `k=v`), e.g. a vendor API key (secret → Secret Manager); masked in the config log view | — |
| `OTEL_TRACES_SAMPLER` | standard OTel sampler; trace volume is one-per-webhook, so always-on is correct (cost is spans-per-trace, not trace rate) | `parentbased_always_on` |
| `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` | opt-in capture of prompt/response **bodies** as span attributes (sensitive — reviewed source code); the standard GenAI-semconv var the framework reads natively; model/token/tool/latency attributes are captured free regardless | `false` |

The full env reference (including SDK-owned Vertex/AI-Studio vars) lives in
[`DEPLOYMENT.md`](../../DEPLOYMENT.md).

---

## 13. Deployment

Target: **Cloud Run + Firestore** (the durable-session path), or a persistent instance for the
in-memory mode. The full ops walkthrough — Firestore setup, ADC roles, Cloud Scheduler jobs,
the firestore emulator for local tests, and the pending-work list — lives in
[`DEPLOYMENT.md`](../../DEPLOYMENT.md); the design-level summary:

```
 GitHub repo ──webhook(HMAC)──► POST /webhooks/{lint,coverage,github}
 Cloud Scheduler ─bearer─►       POST /internal/cron/daily            (daily digest)
 Cloud Scheduler ─bearer─►       POST /internal/sweep                 (timeout catch-all)
                                         │
                              managed API gateway   (single ingress: authn, rate-limit, routing)
                                         │
                                    Cloud Run service (this app)
                                         │
                       webhook returns fast ─► enqueue on the execution transport
                                         │
                       TASKS_BACKEND = inprocess | cloudtasks
                          inprocess: in-process background worker pool (local/default)
                          cloudtasks: Cloud Tasks ─bearer─► POST /internal/dispatch
                                         │              (runs the workflow IN-REQUEST)
                       ┌─────────────────┴─────────────────┐
                  session.Service                       ParkStore
                  (suspend/resume history)         (prKey→session, attempts, params)
                  memory | sqlite | firestore     memory | sqlite | firestore
```

- **Execution transport (`TASKS_BACKEND`).** A webhook returns fast, then its multi-minute LLM
  compute runs **in-request** — on Cloud Run, CPU is throttled once the response is sent, so post-202
  background work is starved and a mid-run instance reclaim loses it. `cloudtasks` (prod) enqueues each
  envelope as a Cloud Tasks HTTP-target task → `POST /internal/dispatch` (Bearer `INTERNAL_TOKEN`),
  which runs the workflow synchronously with CPU allocated; the queue adds durable retry + rate
  limiting. `inprocess` (local/default) reproduces the in-process worker pool. **Scale-to-zero is
  preserved** (no `min-instances`). Orthogonal to the fixers' durable CI wait (that offloads *waiting*;
  this fixes *computing*). See `specs/20260626-workflow-execution-transport.md`.
- **Observability (`OTEL_TRACES_EXPORTER`).** The agent framework already emits a native
  span tree per run (`invoke_agent` → `call_llm` → `execute_tool`, GenAI semantic
  conventions); the `internal/obs` package registers the tracer provider + exporter that
  turns it on, propagates the trace across the Cloud Tasks boundary (a `traceparent` header
  on the task; a `context` passthrough for the in-process backend), and **force-flushes
  spans before each traced handler returns** — load-bearing on scale-to-zero, the same CPU
  throttling that made the execution transport run in-request also starves the async span
  export, so the buffered batch must be pushed out while CPU is still allocated. Off by
  default (`none`); enable per-environment (`gcp` for Cloud Trace, or `otlp` at any backend /
  a Collector). Design in [`observability.md`](observability.md).
- **Prod (scale-to-zero): Cloud Run + `SESSION_BACKEND=firestore`.** Because firestore makes
  parked runs durable, the instance no longer has to stay warm to avoid stranding work — it can
  scale toward zero and rehydrate a parked run when CI reports. ADC gives the service account
  `roles/datastore.user` (Firestore) and `roles/aiplatform.user` (Gemini-on-Vertex); no keys.
- **Cloud Scheduler** drives `/internal/cron/daily` (the daily digest) and `/internal/sweep`
  (durable timeout catch-all), each Bearer-authed with `INTERNAL_TOKEN`. It is the only
  trigger — the service runs no in-process cron, so there is no double-fire to guard against.
- **Lightweight mode: `SESSION_BACKEND=memory`** (default) on a persistent instance
  (`min-instances=1` or a GCE VM) keeps the old zero-storage behavior — but a restart strands
  parked runs, so avoid redeploying while runs are parked.
- Secrets → **Secret Manager**, not plain `.env`.
- Model in prod → likely `LLM_PROVIDER=gemini` (Vertex) unless we provision a GPU VM for
  Ollama. Config flag, no code change.

**Private ingress.** For a deployment that must stay off the public internet, the service runs
**private** (`ingress=internal-and-cloud-load-balancing`) behind an Internal Application Load
Balancer, with a **self-hosted API gateway** on the operator's own network as the single front
door. The gateway is the IAM-authenticated caller — it enforces the webhook edge policies (HMAC,
GitHub IP allowlist, replay window, rate-limit) and presents a Google OIDC token to `/internal/*`
(`INTERNAL_AUTH=oidc`), so a private Cloud Run still receives webhook-originated traffic and the
shared bearer goes away. The HTTP contract is identical across ports, so the gateway config is
port-agnostic. Architecture detail in
[`deployment.md` § Private ingress](deployment.md#private-ingress); rollout intent in
[`DEPLOYMENT.md`](../../DEPLOYMENT.md).

---

## 14. System composition

The system is composed of independently testable layers:

1. **Skeleton & standards** — repo tree, go.mod, Makefile, `.agents/` (standards +
   templates), ARCH tests, AGENTS.md, config, ingest envelope.
2. **Model layer** — `setup`: Ollama adapter + Gemini factory + `BuildLLM` + prompt loader
   + runner. The adapter is tested against a stub Ollama HTTP server.
3. **Tooling** — `githubapi`, `gitrepo`, `notify`, `webhook`; all unit-tested and agent-free.
4. **Root + Summary** — the end-to-end summary workflow runs on a real repo via local Gemma →
   Slack/Teams.
5. **Fixflow-based fixers** — the suspend/resume workflow for lint and coverage.
6. **Deployment** — Cloud Run or GCE, with the Ollama-on-GPU vs Gemini choice as a config flag.

**Durable sessions:**

- The Firestore + Cloud Run durable-session approach is used over Agent Runtime + Cloud SQL.
- A `session.Service` abstraction backs the `SESSION_BACKEND` switch (memory|sqlite|firestore).
- The `ParkStore` interface backs parked runs with memory/sqlite/firestore backends, UUID
  session ids, and an atomic single-winner claim (it replaces an in-memory registry/`runs` map).
- Eager terminal cleanup (`DeleteSession`) plus status-aware terminal summaries
  (success / max-iter / timeout) enrich notifications via `githubapi.Compare`.
- Cloud Scheduler ingress drives `/internal/cron/daily` + `/internal/sweep` (durable timeout
  catch-all), Bearer-authed via `INTERNAL_TOKEN`.
- The ports stay in lockstep on the durable-session design; per-port parity is tracked per-PR
  (see [`language-parity.md`](language-parity.md)).

Not yet implemented: orphan-session GC (sessions that crash between create-and-park),
Terraform/IaC for Firestore + Cloud Run + Scheduler + Secret Manager, and CI running the
Firestore emulator — see [`DEPLOYMENT.md`](../../DEPLOYMENT.md).

---

## 15. Open questions

1. **Persistence — durable sessions.** One `SESSION_BACKEND` env selects
   the ADK `session.Service` + `setup.ParkStore`: `memory` (default, non-durable — the old
   behavior) | `sqlite` (durable local) | `firestore` (durable cloud, scale-to-zero). With a
   durable backend a restart resumes parked runs; GitHub still holds the durable PR artifacts.
   Per-port parity is tracked per-PR (see [`language-parity.md`](language-parity.md)). See §8.
2. **Notify.** The `Notifier` interface has both Slack and Teams impls; choice is one
   env var. Teams targets the new **Workflows/Adaptive Card** format (O365 connectors
   deprecating).
3. **Root routing.** Routing starts deterministic; LLM routing can be added later.
4. **Lint-fixer.** The detailed suspend/resume implementation is covered in §8.
5. **CI provider — GitHub Actions / Checks API.** Resume listens for `check_run`
   (completed) on a dedicated, **label-triggered** agent verification check (`synchronize`
   re-runs it each iteration). See §8.
6. **Same-PR vs new-PR on retry — same PR/branch.** Iterations push new commits to the
   existing branch. The correlation key is therefore `pr_number`, with `current_head_sha` as a
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
  embedded-`agent.Config` shape — to confirm against their example dirs.
- adk-go has **no** durable *workflow* engine; durability is supplied at the session layer
  instead. `IsLongRunning` (the long-running `await_ci` tool) over a `SESSION_BACKEND`-selected
  `session.Service`, plus the `setup.ParkStore` for the run record, is the suspend/resume
  mechanism. adk-go ships inmemory/database/vertexai session services; the **firestore**
  `session.Service` is a custom impl in `internal/agent/setup`.

### ADK Sessions — concept references

The `session.Service` / state / events model above is ADK's own Sessions abstraction; our
backend tiers (`memory` → `sqlite` → `firestore`) mirror its InMemory → Database → Vertex
tiers. Canonical docs (verify against current sources — surfaces move):

- ADK **Sessions** concept (`Session`/`State`/`Events`/`SessionService`): <https://adk.dev/sessions/>
- ADK **agent-memory** codelab (sessions/state vs. long-term Memory; `DatabaseSessionService`
  with `sqlite:///`; Vertex AI Memory Bank): <https://codelabs.developers.google.com/codelabs/agent-memory/instructions>.
  We persist **sessions**; the codelab's searchable cross-session **Memory Bank /
  `MemoryService`** tier is not part of this architecture.
