---
type: Decision
title: The OKF knowledge bundle
description: Why system knowledge consolidated into the okf/ bundle with machine-checked conformance, replacing the per-directory AGENTS.md tree and .agents/standards.
tags: [decision, knowledge, okf, documentation]
sensitivity: internal
bundle: automation-agent
status: accepted
decided: 2026-07-04
timestamp: 2026-07-04T00:00:00Z
---

# The OKF knowledge bundle

## Context

System knowledge lived in ~94 per-directory `AGENTS.md` files plus `.agents/standards/`.
The tree duplicated facts at every level, drifted whenever code moved, taxed every
change with many small doc edits, and — being scattered and format-free — could not be
lifted into any shared, queryable knowledge store.

## Decision

Consolidate all system knowledge into the **`okf/` bundle** (Open Knowledge Format:
markdown concepts with YAML frontmatter, cross-linked, one directory of concepts per
layer — orientation / standards / decisions / modules / tooling):

- The **repo-root `AGENTS.md`** is the only agent-instruction file: the auto-loaded
  guardrail sheet plus deep pointers into the bundle. Every per-directory `AGENTS.md`
  and `.agents/standards/` was deleted.
- **Conformance tests replaced doc-presence tests** in every port's architecture suite:
  frontmatter `type` on every concept, `index.md` per directory, bundle-absolute links
  resolve, skill knowledge citations resolve, and the root `AGENTS.md` points at the
  bundle index.
- Every concept carries **fabric fields** (`bundle`, `sensitivity`) and stays
  self-contained, so the bundle can later be published into a shared multi-repo
  knowledge fabric without rework.
- **Skills cite the bundle one-way** — procedures reference concepts for house rules;
  concepts never reference skills.

The format itself is specified in [OKF format](/standards/okf-format.md); where
knowledge lives is governed by [Documentation & diagrams](/standards/documentation.md).

## Consequences

- One fact lives in one place; a change updates one concept plus its index entry.
- Structural rot (dangling links, missing indexes, missing frontmatter) fails CI in
  every port; semantic freshness remains an authoring responsibility.
- Readers navigate by progressive disclosure (index → concept) instead of directory
  proximity.

## Alternatives considered

- **Keep the AGENTS.md tree** — rejected: measured drift and duplication; presence
  tests verified files existed, not that they were true or consistent.
- **A rendered docs site** — rejected: adds a build and publish surface without making
  the knowledge more machine-consumable; agents and conformance tests read markdown
  directly.
