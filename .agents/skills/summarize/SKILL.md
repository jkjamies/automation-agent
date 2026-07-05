---
name: summarize
description: Generate a structured briefing on the whole system, a workflow agent, a language port, or a standard, driven by the okf/ knowledge bundle. Use for onboarding, context-switching, or before reviewing unfamiliar code.
---

# Summarize

Generate a structured briefing about the system or one of its parts. The okf/ bundle is the source — read it first, then confirm against source code.

**Parameters**: target (optional — omit for the whole system; else an agent, port, or standard name)

**Usage examples**:
```text
/summarize              # whole-system overview
/summarize fixflow      # one workflow agent (also: lintfixer, covfixer, reviewer, summary, setup, root-dispatcher)
/summarize python       # one language port (also: go, javascript, kotlin)
/summarize testing      # one standard (any file in okf/standards/)
```

## Reading order

| Target | Read, in order |
|---|---|
| Whole system | `okf/index.md` → `okf/orientation/what-this-system-is.md` → `okf/orientation/event-flow.md` → `okf/orientation/suspend-resume-design.md` → `okf/modules/index.md` |
| Agent (e.g. `fixflow`) | `okf/modules/agents/<name>.md` → every orientation/standard concept it links → the agent's source in the Go reference port |
| Port (e.g. `python`) | `okf/modules/ports/<name>.md` → `okf/standards/language-parity.md` → that port's directory layout and gate (`okf/tooling/ci-gates.md`) |
| Standard (e.g. `testing`) | `okf/standards/<name>.md` → the concepts it links → one code example proving the rule is live |

Unknown target? List `okf/modules/agents/`, `okf/modules/ports/`, `okf/modules/platform/`, and `okf/standards/`, pick the closest match, and say which you picked.

## Output shape

Every briefing answers four things, in this order:

```markdown
# Summary: {target}

## What it is
[1–3 sentences: purpose, why it exists]

## How it works
[The mechanism: event flow, agent topology, or the rule and its enforcement]

## Key contracts
[What other parts rely on: inputs/outputs, invariants, parity obligations,
 gates it must pass — cite the okf/ concept that owns each contract]

## Where the code lives
[Absolute-from-repo-root paths: source dirs, tests, the enforcing arch test if any]
```

For the whole system, add a table of the workflow agents (name, one-line purpose) and the four ports (modern pair `go/` + `python/`, frozen pair `kotlin/` + `javascript/`).

## Key Rules

- **Bundle first, code second** — okf/ is canonical; if code contradicts it, report the drift explicitly rather than silently siding with either.
- **Cite, don't copy** — link okf/ paths for detail instead of restating whole documents.
- **Read, don't guess** — never summarize from memory; every claim traces to a file you opened this session.
- **Be specific** — file paths, type names, link targets.
- **Recency matters** — skim `git log --oneline -15` for changes the bundle may not reflect yet.
- Read-only skill: it produces a briefing, never edits. No verification step required.
