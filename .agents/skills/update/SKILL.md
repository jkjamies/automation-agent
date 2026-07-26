---
name: update
description: Modify existing behavior from a change spec — concept-first reading, implementation, contract preservation, and the knowledge updates. Use when changing how an existing agent, package, or flow behaves.
---

# Update

Apply a behavior change to existing code: read the owning concept before the code,
change the code, keep external contracts
stable unless the spec says otherwise, update the bundle.

**Parameters**: change spec via `@file`, or an inline description naming the unit being
changed and the new behavior.

**Usage examples**:
```text
/update @specs/20260718-summary-weekly-rollup.md
/update fixflow: cap the re-park cycle at MaxIter-1 when CI fails on a doc-only diff
```

## Pre-flight: Grill the Spec

When a spec is given via `@file`, scan it for unresolved markers (`<!-- TODO -->`, empty
`- [ ]` AC items, empty required header fields, `...` table cells, `presumably`/`appears
to` presumptions). If any are present, **stop and run `/grill-me @{spec}` first**, then
resume. In particular, the spec must be explicit about whether any **external contract**
(route, Kind, check name, notify payload, env var name) changes — silence means "no".

## Reference knowledge

- Concept ownership map + factual-docs rule: okf/standards/documentation.md
- Route / Kind / check-name registry (the contract tables): okf/standards/webhooks.md
- Import boundaries that must survive the change: okf/standards/architecture.md
- Wiring/logic split (changes land in `<name>.go`, not the wiring): okf/standards/agent-build-pattern.md
- Test rules and per-port gates: okf/standards/testing.md
- Event path, for changes that move where work flows: okf/orientation/event-flow.md

## Steps

### 1. Locate and read the owning concept — BEFORE the code

Map the unit to its concept and read it first; it states the current design, invariants,
and the diagram your change must keep honest:

| Unit | Concept |
|---|---|
| an agent (`*/agent/<name>/`) | `okf/modules/agents/<name>.md` |
| a tooling package | `okf/modules/platform/<name>.md` |
| the fix loop / park-resume | `okf/modules/agents/fixflow.md` + okf/orientation/suspend-resume-design.md |
| routing / ingress shape | `okf/modules/agents/root-dispatcher.md` + okf/orientation/event-flow.md |

If the code and the concept disagree, flag it — you may be about to "fix" documented,
intended behavior.

### 2. Plan the touch set

From the spec + concept, list the files changing in `go/` and their mirrors in
Confirm which side of the build-agent split
each change lands on: behavior goes in `<name>.go` / `<name>.py`; `agents_setup.*` changes
only when the agent topology itself changes. Prompt changes are `prompts/*.md` edits.

### 3. Implement

Make the change in the reference port. Keep boundaries intact (no env reads outside
`config`, no agent imports in tooling, provider SDKs setup-only). Update or add tests in
the same package — mirror-worthy cases, deterministic, no LLM-content assertions.

### 4. External contracts: keep or re-register

Routes, ingest Kinds, check names, branch/label names, webhook signatures, and
Slack/Teams payload shapes are observed by outside systems — **byte-stable by default**.
If (and only if) the spec explicitly changes one:

- update the tables in okf/standards/webhooks.md **in the same change** (they are the
  registry the code must agree with), and okf/standards/ci-integration.md if CI authors
  are affected;
- remember an external contract is observed by GitHub and by the CI workflows in every
  target repo, so changing one silently breaks every repo already wired up
  **together**, and the PR says so.

### 5. Knowledge update — MANDATORY

A change is not done until the concepts and diagrams that describe it are updated in the
same change (okf/standards/documentation.md). Run `/update-okf`: restate the touched
concepts as the new factual reality (no "changed X to Y" narration), refresh any Mermaid
flow the change invalidates, and sync `.env.example` + the architecture-design config
table for env-var changes.

## Verification

```bash
cd go && make ci            # tidy + vet + arch + test + 80% cover
cd go && make docs-check    # okf bundle conformance
```

For a contract change, additionally grep the repo for the old route/Kind/check name —
nothing stale may remain in code, concepts, diagrams, or `.env.example`.

## Key Rules

- **Concept before code** — the bundle states intent; the diff must not fight it unknowingly.
- **External contracts don't drift by accident** — only an explicit spec line changes
  them, and the okf/standards/webhooks.md registry moves in the same commit.
- **Don't expand scope** — change what the spec asks; log adjacent findings, don't fix them.
- **Tests move with behavior** — changed behavior means changed assertions, and a
  regression test must fail against the pre-fix code.
