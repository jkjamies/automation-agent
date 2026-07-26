---
type: Standard
title: Documentation & diagrams
description: How the repo documents itself — the knowledge bundle as the canonical home, factual docs, and the rule that docs and diagrams move with the code in the same change.
tags: [documentation, diagrams, conventions, knowledge-bundle]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Documentation & diagrams

How this repo documents itself, and the rule that keeps the docs honest: **a change is
not done until every place that describes or draws it is updated in the same change.**
These are binding review criteria.

## Where knowledge lives

- **This bundle (`okf/`) is the canonical home of system knowledge** — orientation,
  standards, module concepts (agents / platform / ports), and tooling. Concepts are
  self-contained: they carry the knowledge; they never defer to a source file for it.
  The format itself (frontmatter schema, type taxonomy, timestamp semantics, link
  convention) is specified in [OKF format](/standards/okf-format.md).
- **Rationale lives with the facts it justifies.** A load-bearing choice gets a short
  *Why* in its owning concept — the constraint that forced it and the alternatives
  rejected. Specs are gitignored and disposable: a choice a spec grilled and locked
  graduates into the owning concept's *Why* before the spec dies.
- **The repo-root `AGENTS.md` is the discovery surface**: the auto-loaded guardrail
  sheet plus deep pointers into this bundle. It carries only the un-skippable
  constraints and the map; narrative belongs here, not there.
- The architecture tests enforce the bundle's *structure* (every concept has a
  frontmatter `type`, every directory has an `index.md`, bundle-absolute links resolve,
  the root `AGENTS.md` points at the bundle index) — **content freshness is on the
  author.**
- The ops runbook (`DEPLOYMENT.md`) and per-port `README.md` files remain thin
  operational surfaces; anything conceptual links into the bundle.
- The bundle's `log.md` is the OKF-reserved chronological history file (a changelog of
  the bundle itself, defined by the format) — it is not a concept and the
  facts-not-status rule does not apply to it.

## Docs are factual, not status trackers

- Bundle concepts describe **how the system works right now**, factually. Never
  annotate them with migration status — no "removed the cron", "Phase D done",
  "pending review", "TODO: weekly". Describe the new reality; let the diff and the PR
  carry the "what changed".
- Progress tracking lives in **specs** and the task list, **not** in concepts.
  **Plan/spec docs (`specs/`, `.agents/templates/`) are exempt** — their job *is* to
  capture intent and status.

## Docs + diagrams move with the code

Adding, removing, or renaming an **agent**, an ingest **`Kind`**, an **ingress route**
(incl. a **webhook route**), a **CI `check_run` name**, or a **platform package** must
update *every place that describes or draws it*, in the same change. The surfaces to
sweep:

- the touched unit's **concept** in this bundle (and its directory `index.md` entry);
- the [event flow](/orientation/event-flow.md) diagrams and the
  [root dispatcher](/modules/agents/root-dispatcher.md) concept;
- [Architecture & Build Plan](/standards/architecture-design.md) (§2 at-a-glance, §13
  deployment) and [Deployment](/standards/deployment.md) topology diagrams;
- [Webhooks & CI check names](/standards/webhooks.md) — the webhook-route +
  CI-check-name registry — and [CI Integration](/standards/ci-integration.md) (both
  tables must stay in lockstep with the engine `Spec`s);
- the repo-root `.env.example` (when the var is carried there) + the
  [Architecture & Build Plan](/standards/architecture-design.md) §12 config table (for
  any new/removed env var).

**Before opening the PR, grep the repo for the old name / route / `Kind`** to confirm
nothing stale remains — diagrams included.

## One bundle, one system

The bundle documents **one system** in one place. A concept is updated in the same change
that alters the behavior it describes — there are no parallel doc trees to keep in step.

## Diagram conventions

- **Mermaid** inside concepts (flowcharts of the package/system flow); **ASCII** for
  the topology diagrams in the standards concepts. Match the style already in the file
  you touch.
- Keep **infrastructure generic** — e.g. the managed **API gateway** that fronts Cloud
  Run is drawn as a single un-named ingress; do not name a specific product in
  committed docs.
- A diagram must not imply something the code no longer does (e.g. an in-process timer
  when the trigger is external). The diagram is part of the contract, not decoration.
