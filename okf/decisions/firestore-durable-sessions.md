---
type: Decision
title: Firestore durable sessions on Cloud Run
description: Why parked runs persist in Firestore behind a session-backend switch on plain Cloud Run, rather than moving to Agent Runtime with Cloud SQL.
tags: [decision, sessions, firestore, cloud-run]
sensitivity: internal
bundle: automation-agent
status: accepted
decided: 2026-06-21
timestamp: 2026-07-04T00:00:00Z
---

# Firestore durable sessions on Cloud Run

## Context

A fix run parks across a 20–40+ minute CI wait. On scale-to-zero infrastructure the
process serving the resume webhook is usually **not** the process that parked, so the
paused workflow state (the ADK session event history) and the park record (PR key →
session/interrupt id, attempt count, serialized run params) must live outside the
process. The candidate managed stacks were plain Cloud Run with a document store, or a
managed agent runtime backed by Cloud SQL.

## Decision

Stay on **Cloud Run** and persist both stores in **Firestore**, behind one
`SESSION_BACKEND` switch (`memory` | `sqlite` | `firestore`) that selects the ADK
session service *and* the ParkStore implementation together:

- `memory` — local default; non-durable (pre-durability behavior).
- `sqlite` — durable local file for single-instance development.
- `firestore` — production: serverless, scale-to-zero-friendly, and natively supported
  by the ADK session service; the ParkStore claim is a Firestore transaction (exactly
  one resolver wins).

Scheduled triggers (daily digest, timeout sweep) come from **Cloud Scheduler** hitting
bearer-gated `/internal/*` endpoints — no in-process cron. The design is
[suspend/resume](/orientation/suspend-resume-design.md).

## Consequences

- A process restart resumes parked runs cleanly; this is the change that made
  scale-to-zero viable.
- No relational schema or connection pool to manage; Firestore bills per operation and
  idles at zero alongside the service.
- Local dev keeps a zero-dependency default (`memory`) and a durable option (`sqlite`)
  without the emulator.

## Alternatives considered

- **Managed agent runtime + Cloud SQL** — rejected: heavier managed footprint and an
  always-provisioned database for a workload that idles most of the day; the ADK session
  service already maps natively onto Firestore.
- **In-process waits / resident instance** — rejected: holds an instance hostage for the
  full CI wait and dies on reclaim.
