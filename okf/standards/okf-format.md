---
type: Standard
title: OKF format
description: The bundle's own format contract — directory layout, frontmatter schema, the type taxonomy, timestamp semantics, link convention, and what the conformance tests enforce.
tags: [okf, format, frontmatter, conventions]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# OKF format

The format contract for this bundle. [Documentation & diagrams](/standards/documentation.md)
governs *where knowledge lives*; this concept governs *what a concept is* — so the
bundle can be authored consistently and later lifted into a shared knowledge store
without rework.

## Layout

One directory per layer, each with an `index.md` for progressive disclosure:

| Directory | Holds | Typical `type` |
|---|---|---|
| `orientation/` | what the system is, how events flow, the glossary | `Architecture`, `Reference` |
| `standards/` | the rules every change obeys | `Standard`, `Architecture` |
| `decisions/` | why load-bearing choices were made | `Decision` |
| `modules/agents/` | the workflow agents + the setup layer | `Agent`, `Workflow` |
| `modules/platform/` | the deterministic platform packages | `Platform-Package` |
| `modules/ports/` | the four language ports | `Port` |
| `tooling/` | CI gates, specs/templates, skills, deploy topology | `Tooling` |

**Reserved files** (no frontmatter required): `index.md` — a directory's map, one
bulleted line per entry (a link to the concept plus its one-line description);
`log.md` — the bundle's chronological changelog (newest first), exempt from the
facts-not-status rule.

## Frontmatter schema

Every concept opens with a YAML frontmatter block. Required fields:

| Field | Meaning |
|---|---|
| `type` | one of the taxonomy values below (the one hard conformance requirement) |
| `title` | the concept's display name; matches the H1 |
| `description` | one line; kept in sync with the concept's `index.md` entry |
| `tags` | lowercase topic list for search/routing |
| `sensitivity` | audience classification; `internal` today (fabric-ready field) |
| `bundle` | `automation-agent` — the owning bundle in a multi-bundle fabric |
| `timestamp` | **last material update** (ISO 8601). Bump it whenever the body changes meaning — a stale timestamp misleads consumers deciding what to trust. Cosmetic fixes don't bump it. |

Optional fields:

| Field | Meaning |
|---|---|
| `resource` | repo-relative path of the unit this concept documents (reference-port path for module concepts); must exist |
| `status` | **Decision concepts only**: `accepted` or `superseded` (link the successor from the body). Lifecycle lives here, never in the body |
| `decided` | **Decision concepts only**: the date the decision was made |

**Type taxonomy**: `Architecture`, `Standard`, `Decision`, `Agent`, `Workflow`,
`Platform-Package`, `Port`, `Tooling`, `Reference`. Adding a new type is a deliberate
act: define it here first.

## Authoring rules

- **Self-contained**: a concept carries the knowledge; it never defers to a source file
  for it ("see the code" is not a concept).
- **Factual, present-tense**: describe how the system works now — no migration status,
  no "done/pending/Phase X" (see [Documentation & diagrams](/standards/documentation.md)).
  Decision records state context and consequences; their lifecycle is frontmatter.
- **Links are bundle-absolute** — `[ingest](/modules/platform/ingest.md)` resolves from
  the bundle root. This is the house convention and the form the conformance tests
  validate; anchors (`#…`) are content and not validated.
- **No skill references** in concepts — skills cite the bundle one-way; the single
  exception is [Agent skills](/tooling/skills.md), which describes the skills system.
- **No `specs/` references** — specs are gitignored dev memory
  ([Specs & templates](/tooling/specs-and-templates.md)); a decision a spec locked that
  must outlive it becomes a `decisions/` record.
- **Every concept has an index entry** in its directory's `index.md`, and a material
  add/remove/rewrite gets a dated `log.md` entry.

## The conformance floor

Every port's architecture suite enforces the structural rules — frontmatter `type`
present, `index.md` per directory, bundle-absolute links resolve, skill knowledge
citations resolve, root `AGENTS.md` points at the bundle index. Structure is the floor;
semantic freshness (is the concept still *true*?) is on the author of every change.
