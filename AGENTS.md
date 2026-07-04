# automation-agent

An event-driven automation service (daily summaries, autonomous lint/coverage fixers
with a PR + CI loop, and a PR reviewer) built on the Agent Development Kit, maintained
as parallel language ports of one design: `go/` (reference) · `python/` · `kotlin/` ·
`javascript/`.

## Knowledge — start here

**The system's knowledge lives in the [`/okf`](okf/index.md) bundle** (Open Knowledge
Format: markdown concepts with YAML frontmatter, cross-linked). Start at
[`okf/index.md`](okf/index.md) and follow links; read only the concepts a task needs.

Before non-trivial work in an area, read its concept:

- What the system is & how events flow → [`okf/orientation/`](okf/orientation/index.md)
- Rules every change obeys (parity, testing, style, docs, webhooks…) →
  [`okf/standards/`](okf/standards/index.md)
- A specific agent/workflow (fixflow, reviewer, summary, dispatcher, setup) →
  [`okf/modules/agents/`](okf/modules/agents/index.md)
- A platform package (config, githubapi, gitrepo, ingest, webhook, notify, obs, tasks,
  auth) → [`okf/modules/platform/`](okf/modules/platform/index.md)
- A language port (layout, build/run/test, quirks) →
  [`okf/modules/ports/`](okf/modules/ports/index.md)

Ops runbook (env, GCP setup, `/internal/*` hooks): `DEPLOYMENT.md`.

## Must obey (un-skippable)

These are enforced by architecture tests and review; violating any of them fails the
gate or the PR:

- **Run the port's local gate before proposing changes**: `make ci` from `go/`,
  `python/`, or `javascript/`; `gradle build` from `kotlin/`.
- **Two-pair parity.** Go↔Python are the modern pair: behavior changes land in Go first
  and are mirrored into Python in the same logical change. Kotlin↔TypeScript are
  feature-frozen (a critical fix touching one lands in both). No parity requirement
  across pairs; external contracts (routes, check names, payloads) match across all
  four. Full rules: [`okf/standards/language-parity.md`](okf/standards/language-parity.md).
- **Import boundaries**: tooling packages never import agents; provider SDKs
  (Ollama/Gemini/genai) only inside the port's `agent/setup` layer; nothing imports
  `cmd`; the config layer is the only place that reads environment variables.
- **Testing**: coverage ≥ 80 % per port; never assert on LLM output content; tests stay
  deterministic.
- **No cross-language mentions in port code/comments** — code in one port never names
  another stack (no "mirrors the Go version", no goroutine/asyncio references in the
  other language). Repo-level docs (this file, `/okf`) are exempt.
- **Kotlin: never use the `!!` not-null assertion** (use `requireNotNull`/`?:`/
  `shouldNotBeNull` instead).
- **Prompts are markdown** under each agent's `prompts/` directory, loaded from embedded
  resources — never inline prompt strings in code.
- **Docs are factual, never status trackers** — no "done/pending/Phase X" outside
  `specs/`.
- **Specs**: non-trivial changes start with `make spec name=<slug> kind=<add|remove|change|migrate>`
  (templates in `.agents/templates/`); `specs/` is gitignored dev memory and is never
  referenced from code, standards, or `/okf`.
