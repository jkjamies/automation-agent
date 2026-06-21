# automation-agent (TypeScript / ADK)

An automation service built on the Agent Development Kit (`@google/adk`). Ingest
(cron / webhook / future hooks) flows into a **root agent** (dispatcher) which routes
to one of three workflow agents:

- **summary** — commit digests posted to Slack/Teams.
- **lintfixer** — autonomous lint remediation with a PR + CI loop.
- **covfixer** — test-coverage remediation, sharing the `fixflow` engine.

The PR + CI suspend/resume loop runs on ADK long-running tools plus an in-memory
parked-run registry (no durable store; a restart strands in-flight runs). Deterministic,
agent-free tooling lives under `src/` and is called by agents but never imports them.

The authoritative, language-neutral design is [`docs/architecture.md`](../docs/architecture.md).

## Conventions (enforced by `arch/` + `make ci`)

- **Every directory has an `AGENTS.md`.** Agent directories use one shared doc covering
  both `agentsSetup.ts` (wiring) and the testable logic files.
- **Build-agent pattern:** `agentsSetup.ts` is pure wiring (`build<Name>Agent`); the
  logic files hold the testable behavior.
- **Import boundaries:** tooling must not import `src/agent/...`; provider SDKs (the
  Ollama adapter / Gemini / genai) only in `src/agent/setup`; nothing imports `cmd`.
- **Prompts are markdown** under each agent's `prompts/` dir, loaded from disk.
- **Testing:** ≥80% coverage (`make cover`). Never assert on LLM output content.
- **Models:** agents receive a `BaseLlm` from `setup.buildLLM`; default to local Ollama
  Gemma. Do not hardcode a provider in agents.

## Working here

- `make help` lists targets. `make ci` is the full local gate.
- Lint/type-check via `eslint` + `tsc --noEmit`; coverage measured over `src/`.
- The app runs directly from TypeScript via `tsx` — there is no build step.
