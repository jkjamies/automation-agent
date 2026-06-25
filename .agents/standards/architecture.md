# Architecture rules

The authoritative design is `.agents/standards/architecture-design.md`. This file states the rules the
`ARCH/` suite enforces. The **import-boundary** rules are **language-neutral**: they hold
identically across every port (`go/`, `python/`, `kotlin/`, `javascript/`). The
**durable-session state** model below is the design for the fix-loop's suspend/resume state.
See `.agents/standards/language-parity.md` for the cross-language 1:1 contract; any
deliberate divergence between ports is recorded in the PR that introduces it.

## Flow

Ingest (cron / webhook / future hooks) → `ingest.Envelope` → **root agent** →
**summary**, **lintfixer**, or **covfixer** workflow → Slack/Teams.

## Import boundaries (enforced by `ARCH/`)

1. **Tooling must not import agents.** `internal/{githubapi,gitrepo,webhook,notify}`
   may not import `internal/agent/...`. Tooling is
   deterministic and reusable; agents depend on tooling, never the reverse.
2. **Provider SDKs are confined to `internal/agent/setup`.** Only `setup` may
   import Ollama/Gemini/genai; agents receive a `model.LLM` interface. The same boundary
   covers the **durable-session SDKs** — `glebarez/sqlite`, `gorm`, and
   `cloud.google.com/go/firestore`: they back the sqlite/firestore session + park stores and
   live setup-only. Agents and the Driver depend on the `session.Service` / `setup.ParkStore`
   interfaces, never the SDKs.
3. **Nothing imports `cmd/...`.** Entrypoints are leaves.
4. **Only `internal/config` reads the environment.**

## State

The fix-loop's (lint + coverage) suspend/resume state lives in **two provider-switched stores**,
both confined to `internal/agent/setup` and selected by one `SESSION_BACKEND` env
(`memory`|`sqlite`|`firestore`):

- the ADK **`session.Service`** — the suspend/resume event history, and
- the **`setup.ParkStore`** — the park record (`prKey→sessionID`, attempts, serialized run
  params). The `fixflow` Driver holds this interface, **not** an in-process registry.

Suspend/resume rides on ADK long-running tools; a per-run `CI_TIMEOUT` timer fast-paths each
wait and the durable `ParkStore.Sweep` (Cloud Scheduler → `/internal/sweep`) is the restart-safe
catch-all. Every claim (`ResolveByPRKey`/`Sweep`) is an atomic single-winner operation
(mutex / sqlite CAS / firestore txn). With a durable backend (`sqlite` local, `firestore` cloud)
a process restart **resumes** parked runs — the change that unlocks Cloud Run scale-to-zero;
the `memory` default keeps the old non-durable behavior (a restart drops in-flight runs).
GitHub still holds the durable PR artifacts (PR + label + check/SHA history) but is not scanned
to recover in-flight state. See `.agents/standards/architecture-design.md` §8 and
`DEPLOYMENT.md`.

The ARCH boundary names Go SDKs (`glebarez/sqlite`, `gorm`, `cloud.google.com/go/firestore`)
as the worked example; each port confines its own backend SDKs the same way (e.g. Python's
`aiosqlite` + `google-cloud-firestore` in `agent/setup`, with adk's native session services).
