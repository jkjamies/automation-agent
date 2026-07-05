---
name: update-okf
description: Update the okf/ knowledge bundle after a change to the system — the touched concepts, their index entries, the affected diagrams, and the bundle log. Use at the end of any change that alters system shape (an agent, a Kind, a route, a check name, a package, an env var, a port convention), or standalone when the bundle has drifted from reality.
---

# Update OKF

Keep the knowledge bundle true. A change is not done until every concept and diagram that
describes it is updated in the same change — this skill is that step, made mechanical.

**Parameters**: none (derives scope from the working diff), or an explicit concept path.

**Usage examples**:
```text
/update-okf                                  # derive touched concepts from the current diff
/update-okf okf/modules/agents/fixflow.md    # update one concept deliberately
```

## Reference knowledge

- The rules this skill enforces: okf/standards/documentation.md
- The format contract (frontmatter, types, timestamps, links): okf/standards/okf-format.md
- Bundle structure and conventions: okf/index.md
- The registry that must stay in lockstep with code: okf/standards/webhooks.md

## Steps

### 1. Derive the blast radius

From `git diff --name-only` (branch vs its base), map changed code to concepts:

| Changed path | Owning concept |
|---|---|
| `*/agent/<name>/**` (any port) | `okf/modules/agents/<name>.md` |
| `*/internal/<pkg>/**`, `*/automation_agent/<pkg>/**` | `okf/modules/platform/<pkg>.md` |
| a port's build files, Makefile, conventions | `okf/modules/ports/<port>.md` |
| ingest Kinds, routes, check names | `okf/orientation/event-flow.md` + `okf/standards/webhooks.md` |
| park/resume, session/park stores | `okf/orientation/suspend-resume-design.md` |
| env vars | `okf/standards/local-development.md` + the architecture-design config table |

### 2. Update each touched concept

- State the **new reality** factually — never "changed X to Y", never status language
  (per okf/standards/documentation.md).
- Keep the body **self-contained** — no "see the repo file for details".
- **Bump the concept's `timestamp`** (last material update, per
  okf/standards/okf-format.md) whenever the body changes meaning; cosmetic fixes don't
  bump it.
- If the change implements a choice a spec grilled and locked, capture the *why*
  (the forcing constraint + the alternatives rejected) as a short **Why** section in
  the owning concept **before the spec is discarded** — rationale lives with the facts
  it justifies.
- If the concept's one-line `description` changed meaning, update it **and** the matching
  entry in its directory `index.md`.
- New unit (agent/package/port)? Create the concept with the house frontmatter (`type`,
  `title`, `description`, `tags`, `sensitivity: internal`, `bundle: automation-agent`,
  `timestamp`) and add its index entry. Removed unit? Delete concept + index entry, and
  sweep inbound links (`grep -rn "<name>.md" okf/`).

### 3. Update diagrams

Mermaid flows must not imply something the code no longer does. Check the event-flow
diagrams, the dispatcher concept, and the architecture-design topology sections.

### 4. Log the change

Append a dated entry to `okf/log.md` (newest first) when the bundle gains, loses, or
materially rewrites a concept. Routine factual touch-ups don't need a log entry.

### 5. Verify

```bash
cd go && make docs-check     # okf conformance: frontmatter, indexes, links, skill refs
```

Then `grep -rn` the repo for any old name/route/Kind the change retired — stale mentions
in code comments and READMEs are part of this skill's job.

## Key Rules

- **Concepts are the knowledge; skills only cite them.** Never write "run /update-okf"
  (or any skill reference) inside a concept — the bundle must stand alone when lifted
  into a shared knowledge store. The one exception is `okf/tooling/skills.md`, which
  *describes* the skills system factually.
- **Facts, not diffs** — a concept reads as if the current design was always the design.
- **The conformance tests are the floor, not the bar** — they catch structural rot
  (dangling links, missing indexes); only this skill catches semantic rot.
