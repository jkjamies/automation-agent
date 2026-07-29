---
type: Standard
title: Architecture rules
description: The import-boundary, visibility, and durable-session state rules the ARCH test suite enforces.
tags: [architecture, import-boundaries, sessions]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-29T00:00:00Z
---

# Architecture rules

The authoritative design is [Automation Agent — Architecture & Build Plan](/standards/architecture-design.md).
This document states the rules the `ARCH/` suite enforces. The **import-boundary** rules are
structural, not stylistic — they are the reason a workflow can be reasoned about without
reading the whole tree. The **durable-session state** model below is the design for the
fix-loop's suspend/resume state.

## Flow

Ingest (cron / webhook / future hooks) → `ingest.Envelope` → **root agent** →
**summary**, **lintfixer**, or **covfixer** workflow → Slack/Teams.

## Import boundaries (enforced by `ARCH/`)

1. **Tooling must not import agents.** `internal/{githubapi,gitrepo,webhook,notify,tasks,obs}`
   may not import `internal/agent/...`. Tooling is
   deterministic and reusable; agents depend on tooling, never the reverse.
2. **Provider SDKs are confined to `internal/agent/setup`.** Only `setup` may
   import Ollama/Gemini/genai; agents receive a `model.LLM` interface. The same boundary
   covers the **durable-session SDKs** — `glebarez/sqlite`, `gorm`, and
   `cloud.google.com/go/firestore`: they back the sqlite/firestore session + park stores and
   live setup-only. Agents and the driver depend on the `session.Service` / `setup.ParkStore`
   interfaces, never the SDKs.
3. **Nothing imports `cmd/...`.** Entrypoints are leaves.
4. **Only `internal/config` reads the environment.**

## Visibility (enforced by `ARCH/`)

**An exported identifier must appear in its own package's exported signature surface, or be
referred to by another package.** Anything else is exported but unreachable — a contract in
`godoc` that no caller can enter — and the module is closed (everything is under `internal/`
or `cmd/`), so the tree contains every caller there will ever be. This is the rule that keeps
the import boundaries above meaningful: they only hold while a caller cannot reach past a seam
to the implementation behind it. Stated in full, with the two cases that look like violations
and are not, in [Go style § Visibility](/standards/go-style.md#visibility--export-the-seam-not-the-machinery).

## State

The fix-loop's (lint + coverage) suspend/resume state lives in **two provider-switched stores**,
both confined to `internal/agent/setup` and selected by one `SESSION_BACKEND` env
(`memory`|`sqlite`|`firestore`):

- the ADK **`session.Service`** — the suspend/resume event history, and
- the **`setup.ParkStore`** — the park record (`prKey→sessionID`, attempts, serialized run
  params). The `fixflow` driver holds this interface, **not** an in-process registry.

Suspend/resume rides on ADK long-running tools; a per-run `CI_TIMEOUT` timer fast-paths each
wait and the durable `ParkStore.Sweep` (Cloud Scheduler → `/internal/sweep`) is the restart-safe
catch-all. Every claim (`ResolveByPRKey`/`Sweep`) is an atomic single-winner operation
(mutex / sqlite CAS / firestore txn). With a durable backend (`sqlite` local, `firestore` cloud)
a process restart **resumes** parked runs — the change that unlocks Cloud Run scale-to-zero;
the `memory` default keeps the old non-durable behavior (a restart drops in-flight runs).
GitHub still holds the durable PR artifacts (PR + label + check/SHA history) but is not scanned
to recover in-flight state. See [Automation Agent — Architecture & Build Plan](/standards/architecture-design.md) §8
and `DEPLOYMENT.md`.

The ARCH boundary names the backend SDKs it confines explicitly (`glebarez/sqlite`, `gorm`,
`cloud.google.com/go/firestore`); a new storage backend belongs in `internal/agent/setup` behind the
same interfaces, and the test fails if its SDK is imported anywhere else.
