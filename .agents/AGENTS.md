# .agents

The open-standard knowledge directory — the rules and reusable recipes that keep
this repo architecturally sound. This single `AGENTS.md` documents the whole
`.agents/` tree; the subdirectories below do not carry their own.

## Flow

```mermaid
flowchart TD
    Agents[".agents/ (single AGENTS.md documents the tree)"] --> Std["standards/ — binding rules"]
    Agents --> Skl["skills/ — reusable task recipes"]
    Agents --> Tpl["templates/ — spec templates"]

    Std --> S1["go-style.md"]
    Std --> S2["testing.md (how to run every test kind; >=80% coverage, never assert LLM output)"]
    Std --> S3["agent-build-pattern.md (agents_setup.go vs <name>.go)"]
    Std --> S4["architecture.md (import boundaries, ingest->root->workflow)"]
    Std --> S5["local-development.md (run modes, env vars, container)"]
    Std --> S6["deployment.md (cloud architecture, GCP setup — source of truth)"]
    Std --> S7["language-parity.md · ci-integration.md · architecture-design.md"]
    Std -->|enforced by| Enf["ARCH/, make ci, .golangci.yml"]

    Skl --> K1["add-workflow-agent.md"]
    Skl --> K2["add-ingest-source.md"]
    Skl --> K3["add-tool.md"]

    Tpl --> T1["add.spec.md"]
    Tpl --> T2["remove.spec.md"]
    Tpl --> T3["change.spec.md (default kind)"]
    Tpl --> T4["migrate.spec.md"]

    Cmd["make spec name=&lt;slug&gt; kind=&lt;add|remove|change|migrate&gt;"] --> Pick["src = .agents/templates/$kind.spec.md"]
    Pick -->|missing| Fail["echo 'no template' -> exit 1"]
    Pick -->|found| Copy["cp src specs/$(date +%Y%m%d)-$name.md"]
    Copy --> Out["specs/ (gitignored scratchpad)"]
    T1 -.-> Pick
    T2 -.-> Pick
    T3 -.-> Pick
    T4 -.-> Pick
```

## `standards/` — binding rules

The rules of the codebase. Enforced where possible by `ARCH/`, `make ci`, and
`.golangci.yml`; otherwise treat them as required review criteria.

- `go-style.md` — formatting, naming, error handling, package design.
- `testing.md` — how to run every kind of test per port (Go current); the ≥80% coverage
  and "never assert LLM output" rules.
- `local-development.md` — prerequisites, run modes, env-var reference, local container.
- `deployment.md` — **source of truth** for cloud architecture + GCP setup (root
  `DEPLOYMENT.md` is a thin status/checklist pointer back here).
- `ci-integration.md` — how a CI workflow drives the lint/coverage fixers.
- `agent-build-pattern.md` — the `agents_setup.go` (wiring) vs `<name>.go` (logic) split.
- `architecture.md` — import boundaries and the ingest→root→workflow flow.
- `architecture-design.md` — the authoritative language-neutral design.
- `language-parity.md` — the 1:1 cross-port contract (Go is the reference).

The how-to-run docs document the design and how each port runs/tests/deploys; the ports
are kept at 1:1 parity per `language-parity.md` (Go is the reference).

When standards and convenience conflict, the standards win.

## `skills/` — reusable task recipes

Step-by-step recipes for common tasks, so work is consistent regardless of who or
what performs it. Populated as the codebase grows. Planned:

- `add-workflow-agent.md` — scaffold a new agent dir using the build-agent pattern.
- `add-ingest-source.md` — wire a new `ingest.Kind` and its handler.
- `add-tool.md` — add a deterministic tooling package and its tests.

## `templates/` — spec templates

Copy one into `specs/` to plan a change before writing code:

```bash
make spec name=<slug> kind=<add|remove|change|migrate>
```

- `add.spec.md` — introduce new functionality.
- `remove.spec.md` — delete functionality safely.
- `change.spec.md` — modify existing behavior.
- `migrate.spec.md` — move/restructure (data, layout, dependency, provider).

`specs/` is gitignored developer memory — scratchpads for working through a change,
not committed artifacts. These templates are the committed, shared starting points.
