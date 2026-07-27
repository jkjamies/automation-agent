---
type: Tooling
title: Deployment topology
description: The service deploys per-port on scale-to-zero Cloud Run behind a managed API gateway, with Cloud Scheduler cron, Cloud Tasks transport, Firestore state, and GitHub App auth.
resource: DEPLOYMENT.md
tags: [deployment, cloud-run, gcp]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

The production shape (the ops runbook with exact
commands and environment reference is `DEPLOYMENT.md` at the repo root):

- **Cloud Run (scale-to-zero)** hosts the service. Nothing in the design requires a
  resident process: parked fix runs are passive data in the session store (see
  [suspend/resume design](/orientation/suspend-resume-design.md)), and multi-minute LLM
  compute runs in-request via the Cloud Tasks transport
  ([tasks](/modules/platform/tasks.md)).
- **A managed API gateway** is the single ingress: authentication, rate limiting, and
  routing for the webhook and internal endpoints
  ([event flow](/orientation/event-flow.md)).
- **Cloud Scheduler** drives time: the daily summary cron (`POST /internal/cron/daily`)
  and the durable timeout sweep (`POST /internal/sweep`).
- **Firestore** (`SESSION_BACKEND=firestore`) backs both the ADK session service and the
  ParkStore in production; `sqlite` and `memory` serve local development
  ([local development standard](/standards/local-development.md)).
- **GitHub App authentication** in production (single-org, pinned installation); a PAT
  is the local-dev fallback only. Tokens stay off disk.
- **Models**: Gemini/Vertex in production; local development runs Ollama + Gemma. One
  builder seam, no provider hardcoding ([setup](/modules/agents/setup.md)).
- **Observability**: OpenTelemetry throughout — exporter selected by env
  (`none`/`console`/`otlp`/`gcp`), with a force-flush guard for scale-to-zero
  ([observability standard](/standards/observability.md)).
