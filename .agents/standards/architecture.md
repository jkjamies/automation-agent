# Architecture rules

The authoritative design is `docs/architecture.md`. This file states the rules the
`ARCH/` suite enforces. These rules are **language-neutral**: they hold identically in
the Go reference and in every port (`kotlin/`, `python/`). See
`.agents/standards/language-parity.md` for the cross-language 1:1 contract.

## Flow

Ingest (cron / webhook / future hooks) → `ingest.Envelope` → **root agent** →
**summary**, **lintfixer**, or **covfixer** workflow → Slack/Teams.

## Import boundaries (enforced by `ARCH/`)

1. **Tooling must not import agents.** `internal/{githubapi,gitrepo,webhook,notify,
   scheduler}` may not import `internal/agent/...`. Tooling is
   deterministic and reusable; agents depend on tooling, never the reverse.
2. **Provider SDKs are confined to `internal/agent/setup`.** Only `setup` may
   import Ollama/Gemini/genai; agents receive a `model.LLM` interface.
3. **Nothing imports `cmd/...`.** Entrypoints are leaves.
4. **Only `internal/config` reads the environment.**

## State

The fix-loop (lint + coverage) keeps **no durable store**. In-flight runs live in an
**in-memory parked-run registry**; suspend/resume rides on ADK long-running tools, and
a per-run `CI_TIMEOUT` timer bounds each wait. GitHub holds the durable PR artifacts
(PR + label + check/SHA history) but is **not** consulted to recover in-flight state —
a process restart strands parked runs (an accepted trade-off; crash recovery is out of
scope). A database / shared registry is a scale-out concern only. See
`docs/architecture.md` §8.
