---
type: Reference
title: Glossary
description: The system's recurring terms — envelope, Kind, park/resume, ParkStore, spec, pair, transport — defined in one place.
tags: [orientation, glossary]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

- **Envelope** — the normalized event: `{Kind, Source, Payload}`. Every ingress source
  produces one; the [root dispatcher](/modules/agents/root-dispatcher.md) routes by its
  `Kind`. See [ingest](/modules/platform/ingest.md).
- **Kind** — the envelope's routing discriminator (`cron.daily`, `lint`, `coverage`,
  `ci`, `review`). Adding a source means adding a Kind, not a new code path.
- **Workflow (agent)** — one of the four top-level units the dispatcher can start:
  [summary](/modules/agents/summary.md), [lint-fixer](/modules/agents/lintfixer.md),
  [coverage-fixer](/modules/agents/covfixer.md), [reviewer](/modules/agents/reviewer.md).
- **fixflow** — the shared engine behind both fixers: the apply → park → resume → retry
  loop plus apply mechanics. Each fixer is a thin **Spec** over it. See
  [fixflow](/modules/agents/fixflow.md).
- **Spec (fixflow)** — a fixer's parameterization of the engine: its triage and analyze
  functions, branch name, PR label, and CI check name.
- **Park / resume** — the durable pause across the CI wait and its webhook-driven
  continuation. See [suspend/resume design](/orientation/suspend-resume-design.md).
- **ParkStore** — the durable park-record store (`owner/repo#pr` → session id, interrupt
  id, attempt count, serialized run params) with an atomic single-winner claim.
- **Park record** — one parked run's row in the ParkStore.
- **Interrupt id** — the identifier of a paused workflow run's request-input pause; a
  resume targets it. Stored in the park record (historically named `call id`).
- **Session backend** — the `SESSION_BACKEND` switch (`memory`|`sqlite`|`firestore`)
  selecting both the ADK session service and the ParkStore implementation.
- **Execution transport** — the `TASKS_BACKEND` switch deciding where webhook-triggered
  compute runs: `inprocess` worker pool locally, or `cloudtasks` re-entering
  `POST /internal/dispatch` so LLM compute runs in-request on Cloud Run. See
  [tasks](/modules/platform/tasks.md).
- **Sweep** — the durable timeout catch-all (`POST /internal/sweep`, Cloud Scheduler):
  claims parked runs whose CI never reported and frees them with a human-review notice.
- **Modern pair / frozen pair** — the parity structure: Go+Python evolve together on the
  ADK 2.x line; Kotlin+TypeScript are feature-frozen, 1:1 with each other. See the
  [parity standard](/standards/language-parity.md).
- **Reference port** — the Go implementation; behavior changes land there first and are
  mirrored into Python in the same logical change.
- **Verify check** — the label-triggered GitHub Actions check (`agent-lint-verify`,
  `agent-coverage-verify`) whose `check_run` conclusion resumes a parked fix run.
- **Clean / no-work** — a triage that found nothing to address: the run concludes
  without opening a PR and sends a positive "already clean" notice, never the
  human-review alarm.
- **Scorecard (reviewer)** — the count-based verdict summary the reviewer posts with its
  advisory review.
- **Spec (repo process)** — a design/intent document under `specs/` (gitignored dev
  memory). See [specs & templates](/tooling/specs-and-templates.md).
- **Bundle (OKF)** — this `okf/` directory: the canonical knowledge for the system,
  written as Open Knowledge Format concepts. Format contract:
  [OKF format](/standards/okf-format.md).
- **Concept** — one markdown file in the bundle: YAML frontmatter (`type`, fabric
  fields) plus a self-contained, factual body; a load-bearing choice carries its *Why*
  (constraint + rejected alternatives) in the concept that owns it.
- **Conformance tests** — the per-port architecture-suite checks that gate the bundle's
  structure (frontmatter `type`, per-directory `index.md`, resolving links and skill
  citations, the root `AGENTS.md` pointer).
