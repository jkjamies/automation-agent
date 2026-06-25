# Documentation & diagrams

How this repo documents itself, and the rule that keeps the docs honest: **a change is
not done until every place that describes or draws it is updated in the same change.**
These are binding review criteria. `ARCH/` `docs_test` enforces only that an `AGENTS.md`
*exists* per directory — **content freshness is on the author.**

## AGENTS.md everywhere

- One `AGENTS.md` per directory, plus the repo root, `cmd/agent`, and `.agents/` (which
  documents its whole tree in a single file). Each port mirrors this.
- Inside an agent directory, a single *shared* `AGENTS.md` documents both the wiring file
  (`agents_setup.*`) and the logic file (`<name>.*`) — see
  [`agent-build-pattern.md`](agent-build-pattern.md).
- Keep entrypoint docs (`cmd/agent/AGENTS.md`) thin — composition only; testable detail
  belongs in the package docs.

## Docs are factual, not status trackers

- Root and `standards/` docs describe **how the system works right now**, factually. Never
  annotate them with migration status — no "removed the cron", "Phase D done",
  "pending parity", "TODO: weekly". Describe the new reality; let the diff and the PR carry
  the "what changed".
- Progress/▢ tracking lives in **specs** and the task list, **not** in package or standards
  docs. **Plan/spec docs (`specs/`, `.agents/templates/`) are exempt** — their job *is* to
  capture intent and status.

## Docs + diagrams move with the code

Adding, removing, or renaming an **agent**, an ingest **`Kind`**, an **ingress route** (incl. a
**webhook route**), a **CI `check_run` name**, or a **tooling package** must update *every place
that describes or draws it*, in the same change. The surfaces to sweep:

- the touched package's own `AGENTS.md`;
- the **root `AGENTS.md` system-flow** mermaid **and** the `agent/root` **dispatcher** diagram;
- [`architecture-design.md`](architecture-design.md) (§2 at-a-glance, §13 deployment) and
  [`deployment.md`](deployment.md) topology diagrams;
- [`webhooks.md`](webhooks.md) — the webhook-route + CI-check-name registry — and
  [`ci-integration.md`](ci-integration.md) (for any new/removed/renamed kickoff route or
  `check_run` name; both tables must stay in lockstep with the engine `Spec`s in every port);
- `.env.example` + the `architecture-design.md` §12 config table (for any new/removed env var);
- the **same diagrams/docs in every port** (see cross-port parity below).

**Before opening the PR, grep the repo for the old name / route / `Kind`** to confirm nothing
stale remains — diagrams included.

## Cross-port parity for docs

Go is the source of truth. The root and `agent/root` `AGENTS.md` diagrams (and the
topology diagrams) carry **parallel content in every port** — when one changes, mirror it to
`python/`, `javascript/`, and `kotlin/` in the same change. See
[`language-parity.md`](language-parity.md).

## Diagram conventions

- **Mermaid** inside `AGENTS.md` (flowcharts of the package/system flow); **ASCII** for the
  topology diagrams in the `standards/` docs. Match the style already in the file you touch.
- Keep **infrastructure generic** — e.g. the managed **API gateway** that fronts Cloud Run is
  drawn as a single un-named ingress; do not name a specific product in committed docs.
- A diagram must not imply something the code no longer does (e.g. an in-process timer when the
  trigger is external). The diagram is part of the contract, not decoration.
