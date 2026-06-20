# Automation Agent — Architecture & Build Plan

> Status: **Design / iterating.** This is the living design doc. Nothing is built yet.
> Last updated: 2026-06-20.

A single long-running Go service that ingests events from many sources, routes every
ingest through a **Root Agent**, and runs two workflow agents: a **Summary** workflow
(daily/weekly commit digests) and a **Lint-fixer** workflow (autonomous lint remediation
with PR + CI feedback loop).

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
                                     ┌────────────┴────────────┐
                                     ▼                         ▼
                          ┌────────────────────┐   ┌──────────────────────────┐
                          │  SUMMARY workflow   │   │   LINT-FIXER workflow     │
                          │ Sequential:         │   │ Loop(max=3):              │
                          │  Parallel[fetch×N]  │   │  Sequential:              │
                          │   → summarize(LLM)  │   │   analyze(LLM)            │
                          │   → notify          │   │   → apply-fix(git/PR)     │
                          └─────────┬──────────┘   │   → suspend → CI resume    │
                                    ▼              │  → notify(summary)         │
                              Slack / Teams        └────────────┬──────────────┘
                                                                ▼
                                                          Slack / Teams
```

Tooling (`git`, `github`, `webhook`, `notify`, `scheduler`, `store`) is **deterministic
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
| State store | *(none — GitHub is the source of truth)* | recovery re-scans labeled PRs; attempt count derived from distinct agent-pushed SHAs. A DB is a scale-*out* concern, not a restart-survival one |

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
│   ├── main.go                    # wire config→store→tooling→runner→scheduler→http; block
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
    │   └── lintfixer/
    │       ├── agents_setup.go    # BuildLintFixerAgent(deps) -> Loop(3)[Sequential[...]]
    │       ├── lintfixer.go       # analyze/apply/resume logic, correlation-id handling
    │       ├── prompts/
    │       │   ├── analyze.md
    │       │   └── summarize_result.md
    │       ├── models/            # payload structs (lint problem, fix attempt, ci result)
    │       └── AGENTS.md
    ├── githubapi/                 # go-github: ListCommits, CreatePR, ListAgentPRs,
    │   │                          #   check status, distinct-SHA attempt count
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
    ├── notify/                    # Slack/Teams behind one interface
    │   ├── notify.go              # Notifier interface
    │   ├── slack.go
    │   ├── teams.go               # plan for Workflows/Adaptive Card (O365 connectors deprecating)
    │   └── AGENTS.md
    └── reconcile/                 # stateless recovery (GitHub is the source of truth)
        ├── reconcile.go           # ticker + startup scan: find AGENT_PR_LABEL PRs,
        │                          #   resume finished ones, time out stuck ones
        └── AGENTS.md
```

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

**Lint-fixer** (`lintfixer/`): `Loop(MaxIterations: 3)[ Sequential[ analyze(LLM) → apply-fix → await-CI ] ]`.
See the dedicated section below — this is the complex one.

---

## 8. Suspend / resume design (CI feedback loop)

> **This section will be refined with the detailed notes from the prior discussion.**
> The structure below is the scaffold those notes drop into.

### The hard constraint: CI takes 20–40 minutes (often more with retries)

A fix can't be confirmed for 20–40 min (×3 iterations → up to ~2 h wall-clock), so the
workflow can't sit in a blocked goroutine. But this does **not** require a local durable
database — because **GitHub already is the durable store**:

- the **PR** exists on GitHub (number, branch, head SHA),
- the **check conclusion** exists on GitHub (the agent verify check),
- the **label** marks it as ours,
- the current **lint findings** are re-derivable by reading the check output / re-running lint.

Even the attempt count isn't really stored: on the happy path the `LoopAgent` tracks it in
memory, and after a crash it's **re-derived from GitHub** as the number of distinct
agent-pushed head SHAs on the PR (re-run-safe — a manual check re-run reuses the same SHA).
The `AGENT_PR_LABEL` just marks the PR as ours so the scan can find it. Consequences:

1. **No local DB, no file, no volume, no retention janitor, nothing to clean up manually.**
   Matches the "lightweight" goal and makes the service stateless/replaceable.
2. **In-memory working state is just a cache.** We keep a small in-memory map of active runs
   for the happy path and as a concurrency guard, but nothing depends on it surviving — it's
   rebuilt from GitHub.
3. **Recovery = re-scan labeled PRs.** On startup and on a timer (`RECONCILE_INTERVAL`), list
   open PRs with `AGENT_PR_LABEL`, read each one's check conclusion, and resume any that
   finished while we weren't listening. Webhook = fast path; scan = catch-all.
4. **Worst case is self-healing.** If we crash *before* the PR exists, the lint payload is
   simply re-produced by the next scheduled CI lint run — the next kickoff covers it. Nothing
   orphaned.
5. **Idempotency via the SHA guard.** Act on a `check_run` only when its `head_sha` equals the
   PR's current head SHA, so duplicate deliveries and stale-iteration checks are ignored — no
   dedupe table needed.
6. **Timeout is derived, not stored.** During the scan, a labeled PR whose agent check has
   been pending longer than `CI_TIMEOUT` → "needs human review" + PR link. Computed from
   GitHub timestamps; no persisted timer.

### Flow

```
lint payload ──▶ root ──▶ lintfixer Loop(max=3)
   │
   │  iteration i:
   │   1. analyze(LLM)   : reason about the lint problem(s), produce a fix plan
   │   2. apply-fix(code): go-git clone/branch/edit/commit/push; go-github open/Update PR
   │                       → this is a LONG-RUNNING tool (IsLongRunning()=true)
   │                       → returns correlation id {pr_number, head_sha, branch, iteration}
   │                       → run YIELDS; PR label + GitHub check/SHA history ARE the state; released
   │
   ▼ (20–40+ min later)
/webhooks/github (check_run) ──▶ lookup run by pr_number
                  guard: event head_sha == run.current_head_sha; require terminal conclusion
                  rehydrate workflow state
                  ┌─ CI success ─▶ finish: post success summary (Slack/Teams) + PR link
                  ├─ CI failure & iteration < 3 ─▶ resume loop: re-analyze WITH ci feedback
                  └─ CI failure & iteration == 3 ─▶ finish: "may have failed, human review needed" + PR link
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

Retries push new commits to the **same** branch/PR (confirmed). Each push creates a **new
`head_sha`**, so `head_sha` is *not* a stable key — **`pr_number` is the stable correlation
key**, and `current_head_sha` is tracked as a freshness guard:

- Match an incoming `check_run` to a run by **`pr_number`** (the event's
  `pull_requests[].number`).
- **Guard:** only act if the event's `head_sha` equals the run's `current_head_sha`. This
  drops late-arriving checks from a previous iteration's SHA.
- On each `apply-fix` push, update `current_head_sha` and `iteration` in the store record.

We persist **nothing locally**. The only durable bit of identity is the `AGENT_PR_LABEL`,
which marks a PR as ours; everything else — `pr_number`, `current_head_sha`, check status,
timestamps — is read live from GitHub, and findings are re-derived from the check output each
iteration.

**Attempt count: in-memory on the happy path, re-derived on recovery.** The `LoopAgent`
bounds the loop (`MaxIterations: 3`) and tracks the current pass in memory — no GitHub read
needed normally. We only reconstruct the count after a crash-mid-wait, deriving it as the
**number of distinct agent-pushed head SHAs** on the PR (each real attempt is one push = one
new SHA; a manual check re-run reuses the same SHA, so it can't inflate the count). The
give-up decision:

- **CI failed and attempt == `MAX_ITERATIONS` (3)** → post the failure summary
  (needs-human-review + PR link) to Slack/Teams and stop.
- **Reconcile scan hits `CI_TIMEOUT`** (check still pending) → same failure summary, timeout
  variant.

Because the count only matters on the rare crash path, an off-by-one there is harmless and
bounded by `MAX_ITERATIONS` — never a runaway loop. The only thing that defeats the
derivation is a human force-push/rebase of the branch — a deliberate intervention where
escalating to human review is the correct outcome anyway.

### Reconcile loop — one stateless ticker (no local state)

A single ticker (`RECONCILE_INTERVAL`, default ~15m) plus a run on startup keeps everything
honest by querying **GitHub**, not a local DB:

- **Catch missed webhooks:** list open PRs with `AGENT_PR_LABEL`; for each, read the agent
  check via go-github `ListCheckRunsForRef(head_sha)`. If it finished while we weren't
  listening, resume now.
- **Timeout / give-up:** if that check has been pending longer than `CI_TIMEOUT` (default
  90m), post "needs human review" + PR link (and optionally drop the label so the scan stops
  revisiting it).
- **No retention/deletion step** — there are no local records to expire. A finished PR is
  merged or closed by the normal review workflow; that *is* the cleanup.

Two layers of safety: **webhook (fast path)** → **reconcile scan (catch-all + timeout)**.

### ADK mechanics

- `apply-fix` is implemented as a tool whose `IsLongRunning()` returns `true` — ADK's
  contract for "return a resource id now, finish later." The run yields after dispatching it.
- Resume reconstructs minimal context from the PR (label + distinct-SHA count → attempt;
  check output → current findings) and starts a fresh runner invocation for the next loop
  step. adk-go has **no** durable engine, and we deliberately don't add one — GitHub is the
  state.

### When a DB / durable engine enters the picture

**Not** for surviving restarts — GitHub already covers that. A datastore becomes worthwhile
only when we cross a scaling threshold:

- **Multiple instances (HA / horizontal scale).** Two replicas could both grab the same PR
  during a scan; that needs a shared lock or work queue GitHub can't provide atomically.
  *Interim option:* a soft lock via an `agent-processing` label + the SHA guard. *Clean
  option:* a small Postgres or a durable engine (Temporal/River).
- **Crash-safe timers / retries with backoff** beyond "the next scan catches it."
- **Audit trail / metrics / cross-PR queuing** beyond what GitHub records.

Whatever we add slots behind the existing **reconcile + CI-handler** seam — the agent code
doesn't change. Single persistent instance: none of this is needed.

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
  - `internal/agent/...` may import `internal/{githubapi,gitrepo,notify,store,config,ingest}`.
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
| `CI_TIMEOUT` | how long a pending check waits before "needs review" | `90m` |
| `RECONCILE_INTERVAL` | how often to re-scan labeled PRs for missed webhooks | `15m` |
| `GITHUB_WEBHOOK_SECRET` | HMAC verify for `/webhooks/github` | — |
| `AGENT_PR_LABEL` | label that triggers the agent verify check | `automation-agent` |
| `AGENT_CHECK_NAME` | check name we resume on | `agent-lint-verify` |

---

## 13. Deployment

Target: a **persistent** GCP instance (always-on for cron + webhooks).

- **Cloud Run** with `min-instances=1` (keeps cron + webhook listener warm), or a **GCE VM**
  if we co-locate Ollama-on-GPU.
- **No persistent disk or database needed** — state lives on GitHub, so the service is
  stateless and freely replaceable/redeployable. We still want it *running* (min-instances=1
  or a VM) to receive webhooks and run the daily cron, but a restart/redeploy loses nothing.
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
3. **Tooling** — `githubapi`, `gitrepo`, `notify`, `store`, `scheduler`, `webhook`.
   *(all unit-tested, agent-free)*
4. **Root + Summary** — end-to-end summary workflow on a real repo via local Gemma →
   Slack/Teams.
5. **Lint-fixer** — the suspend/resume workflow, incorporating the detailed notes.
6. **Deployment** — Cloud Run (min-instances=1) or GCE; decide Ollama-on-GPU vs Gemini.

---

## 15. Open questions

1. **Persistence:** ✅ none — **GitHub is the source of truth** (PR label + hidden body
   marker). No local DB/file; recovery is a stateless re-scan of labeled PRs. See §8.
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
- adk-go has **no** durable workflow engine; `IsLongRunning` + our `store` is the
  suspend/resume mechanism.
