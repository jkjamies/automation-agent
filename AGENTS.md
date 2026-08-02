# automation-agent

An event-driven automation service (daily summaries, autonomous lint/coverage fixers
with a PR + CI loop, and a PR reviewer) built on the Agent Development Kit for Go. The
service lives in `go/`.

## Knowledge — start here

**The system's knowledge lives in the [`/okf`](okf/index.md) bundle** (Open Knowledge
Format v0.2: markdown concepts with YAML frontmatter, cross-linked). Start at
[`okf/index.md`](okf/index.md) and follow links; read only the concepts a task needs.

Before non-trivial work in an area, read its concept:

- What the system is & how events flow → [`okf/orientation/`](okf/orientation/index.md)
- Rules every change obeys (testing, style, docs, security, webhooks…) →
  [`okf/standards/`](okf/standards/index.md)
- A specific agent/workflow (fixflow, reviewer, summary, dispatcher, setup) →
  [`okf/modules/agents/`](okf/modules/agents/index.md)
- A platform package (config, githubapi, gitrepo, ingest, webhook, notify, obs, tasks,
  auth) → [`okf/modules/platform/`](okf/modules/platform/index.md)
- The service's layout, build targets, and conventions →
  [`okf/modules/service.md`](okf/modules/service.md)

Ops runbook (env, GCP setup, `/internal/*` hooks): `DEPLOYMENT.md`.

## Skills — how work is done here

Reusable procedures live in `.agents/skills/<name>/SKILL.md` (described in
[`okf/tooling/skills.md`](okf/tooling/skills.md)). The workflow they encode:
requirements → `/ac-to-spec` (or `/reverse-spec` from code, `/grill-me` on stale specs)
→ implementation (`/add-workflow-agent`, `/add-platform-package`, `/update`, `/migrate`)
→ `/verify` + `/run-firestore-tests` → `/update-okf` (knowledge moves with
the change) → `/review-branch`. `/summarize` gives briefings from the bundle. Prefer a
skill over improvising its procedure.

## Must obey (un-skippable)

These are enforced by architecture tests and review; violating any of them fails the
gate or the PR:

- **Run the local gate before proposing changes**: `make ci` from `go/`. It runs
  tidy-check + vet + lint + arch + test + coverage, and is what CI runs too. It is
  read-only — an untidy `go.mod` is reported, not silently fixed (`make tidy` fixes it).
- **Import boundaries**: tooling packages never import agents; provider SDKs
  (Ollama/Gemini/genai) only inside the `agent/setup` layer; nothing imports
  `cmd`; the config layer is the only place that reads environment variables.
- **Go style**: [Google's Go Style Guide](https://google.github.io/styleguide/go/) is the
  baseline. `make lint` decides everything a linter can — every rule in `go/.golangci.yml`
  cites the guide section it enforces — and `ARCH/` decides the cross-package rules a linter
  cannot see, including what may be exported. The deltas the guide cannot cover are in
  [`okf/standards/go-style.md`](okf/standards/go-style.md).
- **Testing**: coverage ≥ 80 %, enforced per package as well as overall; never
  assert on LLM output content; tests stay deterministic.
- **Prompts are markdown** under each agent's `prompts/` directory, loaded from embedded
  resources — never inline prompt strings in code.
- **Docs are factual, never status trackers** — no "done/pending/Phase X" outside
  `specs/`.
- **Specs**: non-trivial changes start with `make spec name=<slug> kind=<add|remove|change|migrate>`
  (templates in `.agents/templates/`); `specs/` is gitignored dev memory and is never
  referenced from code, standards, or `/okf`.
