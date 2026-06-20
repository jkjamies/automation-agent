# Architecture rules

The authoritative design is `docs/architecture.md`. This file states the rules the
`ARCH/` suite enforces.

## Flow

Ingest (cron / webhook / future hooks) → `ingest.Envelope` → **root agent** →
**summary** or **lintfixer** workflow → Slack/Teams.

## Import boundaries (enforced by `ARCH/`)

1. **Tooling must not import agents.** `internal/{githubapi,gitrepo,webhook,notify,
   scheduler,reconcile}` may not import `internal/agent/...`. Tooling is
   deterministic and reusable; agents depend on tooling, never the reverse.
2. **Provider SDKs are confined to `internal/agent/setup`.** Only `setup` may
   import Ollama/Gemini/genai; agents receive a `model.LLM` interface.
3. **Nothing imports `cmd/...`.** Entrypoints are leaves.
4. **Only `internal/config` reads the environment.**

## State

The lint-fixer keeps **no local durable store** — GitHub is the source of truth
(PR + label + check/SHA history). Recovery is a stateless reconcile scan. A
database is a scale-out concern only. See `docs/architecture.md` §8.
