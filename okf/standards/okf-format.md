---
type: Standard
title: OKF format
description: The bundle's own format contract — the OKF version it targets, directory layout, frontmatter schema, the type taxonomy, provenance and lifecycle fields, link convention, and what the conformance tests enforce.
tags: [okf, format, frontmatter, conventions]
sensitivity: internal
bundle: automation-agent
status: stable
generated: { by: human:jkjamies, at: 2026-08-01T00:00:00Z }
---

# OKF format

The format contract for this bundle. [Documentation & diagrams](/standards/documentation.md)
governs *where knowledge lives*; this concept governs *what a concept is* — so the
bundle can be authored consistently and later lifted into a shared knowledge store
without rework.

## Version

The bundle targets **Open Knowledge Format v0.2**, declared as `okf_version: "0.2"` in the
frontmatter of the bundle-root `index.md` — the one index file allowed to carry
frontmatter at all. Everything below is this bundle's house profile of that spec: the
spec's floor is one required field (`type`) and permissive consumption; the house profile
narrows the type taxonomy, requires more frontmatter than the spec does, and fixes the
link form, because a single-bundle repo can afford consistency the open format cannot
mandate.

## Layout

One directory per layer, each with an `index.md` for progressive disclosure:

| Directory | Holds | Typical `type` |
|---|---|---|
| `orientation/` | what the system is, how events flow, the glossary | `Architecture`, `Reference` |
| `standards/` | the rules every change obeys | `Standard`, `Architecture` |
| `modules/agents/` | the workflow agents + the setup layer | `Agent`, `Workflow` |
| `modules/platform/` | the deterministic platform packages | `Platform-Package` |
| `tooling/` | CI gates, specs/templates, skills, deploy topology | `Tooling` |

**Reserved files**: `index.md` — a directory's map, one bulleted line per entry (a link
to the concept plus its one-line description); `log.md` — the bundle's chronological
changelog (newest first), exempt from the facts-not-status rule. Neither carries
frontmatter, with one exception: the bundle-root `index.md` declares `okf_version` and
`bundle`.

## Frontmatter schema

Every concept opens with a YAML frontmatter block. Required fields:

| Field | Meaning |
|---|---|
| `type` | one of the taxonomy values below (the spec's one hard requirement) |
| `title` | the concept's display name; matches the H1 |
| `description` | one line; kept in sync with the concept's `index.md` entry |
| `tags` | lowercase topic list for search/routing |
| `sensitivity` | audience classification; `internal` today (fabric-ready field) |
| `bundle` | `automation-agent` — the owning bundle in a multi-bundle fabric |
| `status` | lifecycle: `draft` (not yet reviewed), `stable` (ready to consume), `deprecated` (kept for links and history). A concept lands `stable`; `draft` is for knowledge written ahead of the code it describes. |
| `generated` | `{ by: <actor>, at: <ISO 8601> }` — who produced the concept and **when it last materially changed**. Bump `at` whenever the body changes meaning; a stale `at` misleads consumers deciding what to trust. Cosmetic fixes don't bump it. |

Optional fields:

| Field | Meaning |
|---|---|
| `resource` | repo-relative path of the unit this concept documents; must exist, and the conformance tests check that it does |

**Actors** (`generated.by`) take one of three forms: `human:<id>` for a person,
`<producer>/<version>` for an agent or tool, `process:<id>` for an automated process.
Concepts here are authored and curated by a person, so they carry `human:jkjamies`.

**Type taxonomy**: `Architecture`, `Standard`, `Agent`, `Workflow`,
`Platform-Package`, `Tooling`, `Reference`. Adding a new type is a deliberate act:
define it here first.

**Families this bundle does not use.** OKF v0.2 also defines `sources` (provenance with
credibility signals), `verified` (independent confirmation events), `stale_after` (an
absolute expiry date), and the `Attested Computation` type with its `# Computation`
heading. None are populated here, and a field is omitted rather than filled with an
invented value: concepts are written from the code in this repo rather than from external
sources, `generated.by` is already the human who curates them (so a self-issued `verified`
entry would add no signal), and no concept has a known expiry — a concept goes out of date
when the code moves, which the change that moves it is responsible for.

## Authoring rules

- **Self-contained**: a concept carries the knowledge; it never defers to a source file
  for it ("see the code" is not a concept).
- **Factual, present-tense**: describe how the system works now — no migration status,
  no "done/pending/Phase X" (see [Documentation & diagrams](/standards/documentation.md)).
- **Rationale is part of the fact**: a load-bearing choice gets a short *Why* in its
  owning concept — the constraint that forced it and the alternatives rejected — so the
  reasoning survives the disposable spec where it was argued.
- **Links are bundle-absolute** — `[ingest](/modules/platform/ingest.md)` resolves from
  the bundle root. This is the house convention and the form the conformance tests
  validate; anchors (`#…`) are content and not validated.
- **No skill references** in concepts — skills cite the bundle one-way; the single
  exception is [Agent skills](/tooling/skills.md), which describes the skills system.
- **No `specs/` references** — specs are gitignored dev memory
  ([Specs & templates](/tooling/specs-and-templates.md)); a choice a spec locked that
  must outlive it is captured in the owning concept's *Why*.
- **Every concept has an index entry** in its directory's `index.md`, and a material
  add/remove/rewrite gets a dated `log.md` entry under an ISO 8601 (`YYYY-MM-DD`) heading,
  newest first, opening with one of `**Creation**`, `**Update**`, `**Rewrite**`,
  `**Deprecation**`, `**Initialization**`.

## The conformance floor

The architecture suite enforces the structural rules — every concept's frontmatter
parses and carries `type`, `status` from the lifecycle set, and `generated.by`; no
concept still carries the superseded `timestamp`; the bundle root declares
`okf_version`; no other `index.md` carries frontmatter; `index.md` per directory;
bundle-absolute links resolve; every declared `resource` exists; skill knowledge
citations resolve; root `AGENTS.md` points at the bundle index. Structure is the floor;
semantic freshness (is the concept still *true*?) is on the author of every change.
