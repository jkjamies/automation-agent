---
type: Tooling
title: Specs & templates — design intent as dev memory
description: Non-trivial changes start as a spec generated from a template; specs are gitignored working memory, never referenced from code or standards.
tags: [process, specs, templates]
sensitivity: internal
bundle: automation-agent
status: stable
generated: { by: human:jkjamies, at: 2026-07-04T00:00:00Z }
---

New features and non-trivial changes start as a **spec**: a design/intent document under
`specs/` at the repo root, created from a template:

```
make spec name=<slug> kind=<add|remove|change|migrate>
```

Templates live in `.agents/templates/` (`add.spec.md`, `change.spec.md`,
`migrate.spec.md`, `remove.spec.md`). A spec captures context, motivation, scope, design,
decisions to grill, a test plan, and rollout/rollback — and is *grilled* (decision-by-
decision review) before implementation when the change warrants it.

Rules:

- **Specs are disposable dev memory.** `specs/` is gitignored: specs are working
  documents for the humans and agents building a change, not documentation of the
  system. The durable record of *how the system works* lives in this knowledge bundle
  and the standards; the durable record of *what changed* is git history.
- **Never reference a spec from code, standards, or configuration.** Only other specs
  may reference specs. Once a change lands, any knowledge worth keeping moves into the
  bundle or a standard, and the spec is deleted or left to rot harmlessly.
- **Specs may carry status** ("draft", "superseded") — they are the one documentation
  surface where progress language belongs, per the
  [documentation standard](/standards/documentation.md).
