# Porting status — TypeScript / JavaScript port

Go (`go/`) is the reference port. This file maps every reference package to its port
state and records the deliberate gaps, per `.agents/standards/language-parity.md`.

**ADK:** TypeScript + `@google/adk` (adk-js) with a hand-rolled Ollama `BaseLlm` adapter,
mirroring Go's topology. The ADK *wiring* differs from Go; the agent *topology and
behavior* do not.

## Package status

| Reference package | Port (`src/…`) | State | Notes |
|---|---|---|---|
| `config` | `config/config.ts` | ✅ done | Same env vars/defaults/validation as Go. |
| `ingest` | `ingest/envelope.ts` | ✅ done | |
| `webhook` | `webhook/server.ts` | ✅ done | HMAC-gated kickoffs + GitHub resume; oversize body → 413. |
| `scheduler` | `scheduler/scheduler.ts` | ✅ done | Cron pinned to UTC (croner `timezone: 'UTC'`). |
| `notify` | `notify/*.ts` | ✅ done | Slack + Teams Adaptive Card; 10s timeout. |
| `githubapi` | `githubapi/client.ts` | ✅ done | `agentCheck`/`getFileContent` are dead surface kept for parity with Go. |
| `gitrepo` | `gitrepo/repo.ts` | ✅ done | Uses system `git` via `simple-git` (see deliberate gaps). |
| `agent/setup` | `agent/setup/*.ts` | ✅ done | Native adk-js + Ollama adapter (no streaming — see below). |
| `agent/root` | `agent/root/*.ts` | ✅ done | Distinct daily/weekly summary registration. |
| `agent/summary` | `agent/summary/*.ts` | ✅ done | `Sequential[Parallel[fetch×N] → summarize → notify]`; window + title parameterized. |
| `agent/fixflow` | `agent/fixflow/*.ts` | ✅ done | Long-run suspend/resume + in-memory registry; `failApply` notifies on apply errors. |
| `agent/lintfixer` | `agent/lintfixer/*.ts` | ✅ done | Prompts localized (see below). |
| `agent/covfixer` | `agent/covfixer/*.ts` | ✅ done | Prompts localized (see below). |
| `cmd/agent` | `cmd/agent/main.ts` | ✅ done | Bounded + drainable dispatch, HTTP timeouts, graceful shutdown. |

## Deliberate gaps (recorded drift)

- **Prompts are localized, not byte-identical to Go.** `lintfixer/prompts/analyze.md` says
  "senior **software** engineer" (Go: "senior **Go** engineer") and
  `covfixer/prompts/triage.md` lists JS coverage formats (Istanbul/nyc) alongside the
  others. This is intentional: the Go-specific wording is wrong for a TypeScript codebase.
  The other ports still carry the Go wording verbatim; if the contract is later changed to
  per-language localization everywhere, back-port the neutral wording to Go/Kotlin/Python.
- **Ollama adapter has no streaming path** (`agent/setup/ollama.ts`). It ignores `_stream`
  and always sends `stream: false`, yielding one final response. Functionally equivalent
  for this app (callers drain to completion); Go yields partials when `stream=true`.
- **`gitrepo` shells out to system `git`** (`simple-git`) instead of an in-process library.
  Git ops take no cancellation token (Go's `PlainCloneContext` can be cancelled on
  shutdown); a subprocess, not a hung socket, so the risk is low. It also sets an explicit
  committer identity (`-c user.name`/`-c user.email`) where Go lets the committer default;
  the author is the agent in both, and this is not externally load-bearing.
- **`agentCheck` / `getFileContent` are unused in production** (parity with Go's dead
  surface). Resume reads the conclusion from the webhook event; attempt counts live in the
  in-memory `ParkedRun` registry. Kept so the surface matches Go and is available to wire a
  future resume/timeout re-query.

## Known shared gaps (open in Go too — not TS drift)

- **Park-after-PR race (C3).** A `check_run` webhook landing between PR creation and `park`
  finds no parked run and is dropped, leaving only the per-run timeout. Identical to Go's
  ordering; the correct fix is the durable-registry / re-query-on-resume change owned by the
  architecture review, not a local patch.
- **No rate-limit/retry backoff** for GitHub/notifier calls (`B4`), **triage re-runs each
  retry** (`S6`), **unreadable files are skipped silently** (`B9`), and the **ADK
  `ParallelAgent` analyze/summary fan-out** (`S1`) is heavier than a plain `Promise.all`
  but is the intended demonstration of ADK workflow agents. All match Go.
