---
type: Tooling
title: Agent skills — the repo's procedures
description: Reusable agent procedures under .agents/skills/ that chain requirements → spec → grilled decisions → implementation → verification → knowledge updates, each citing this bundle for house rules rather than restating them.
resource: .agents/skills
tags: [skills, workflow, process]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

The repo ships reusable **skills** — procedure files (`.agents/skills/<name>/SKILL.md`)
an agent harness loads as slash-commands. They encode *how work is done here*; this
bundle encodes *what the system is*. The two are linked in exactly one direction:

## The one-way link rule

- **Skills cite concepts** (by bundle path, e.g. `okf/standards/language-parity.md`)
  for every house rule they rely on — they never copy standards content, so a rule
  changes in one place.
- **Concepts never reference skills.** A concept must stand alone when this bundle is
  lifted into a shared knowledge store whose consumers do not have this repo's skills.
  (This concept is the deliberate exception: it *describes* the skills system, which is
  part of the system.)
- **The link is machine-checked**: the bundle conformance tests scan every `SKILL.md`
  for `okf/…` citations and fail the gate when one dangles — the skill↔knowledge edge
  is never hand-maintained. When the bundle later moves behind a knowledge service, the
  citation form changes from a file path to a concept ID on the retrieval tool — the
  identifiers are the same (a concept's ID is its bundle path), so the flip is
  mechanical.

## The workflow the skills encode

```text
requirements ──/ac-to-spec──▶ spec (grilled inline)
existing code ──/reverse-spec──▶ spec (presumptions marked)
stale/hand-written spec ──/grill-me──▶ implementation-ready spec
        │
        ▼
/add-workflow-agent · /add-platform-package · /update · /migrate
        │                                  (Go first → Python mirror; frozen pair untouched)
        ▼
/verify (diff | full | okf) · /run-firestore-tests
        │
        ▼
/update-okf   (concepts + diagrams + indexes move with the change)
        │
        ▼
/review-branch  (gates → CodeRabbit CLI → triage → wait for human approval)
```

Supporting: `/summarize` (briefings read from this bundle — the bundle is the source).

## Conventions every skill follows

- Frontmatter `name` + `description` (with "use when" guidance); Parameters; Usage
  examples; a **Reference knowledge** section listing the concepts it depends on;
  numbered Steps; Key Rules.
- Skills are **thin drivers**: when a rule or command list lives in a concept, the skill
  cites it instead of restating it.
- Implementation skills end with the knowledge step — a change is not done until the
  concepts and diagrams that describe it are updated in the same change
  ([Documentation & diagrams](/standards/documentation.md)).
