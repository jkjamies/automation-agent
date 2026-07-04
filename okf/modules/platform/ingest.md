---
type: Platform-Package
title: Ingress Envelope
description: The normalized Envelope every ingress source reduces to before reaching the root agent, plus its cross-port JSON wire codec for the Cloud Tasks boundary.
resource: go/internal/ingest
tags: [ingress, envelope, wire-contract]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Ingress Envelope

The normalized `Envelope` that every ingress source is reduced to before reaching
the [root agent](/modules/agents/root-dispatcher.md). `Kind` identifies the trigger
(cron.daily, lint, coverage, ci, review); `Payload` carries the raw source body for the
chosen workflow to parse.

## Flow

```mermaid
flowchart TD
    S1["Cloud Scheduler -> POST /internal/cron/daily"] -->|cron.daily| N
    W1["webhook:/lint"] -->|lint, raw lint JSON| N
    W2["webhook:/coverage"] -->|coverage, raw coverage JSON| N
    W3[GitHub check_run webhook] -->|ci, check_run body| N
    W4[GitHub pull_request webhook] -->|review, pull_request body| N
    N["New(kind, source, payload, at)"] --> E["Envelope{Kind, Source, ReceivedAt, Payload}"]
    E --> V{"k.Valid()?"}
    V -->|"cron.daily / lint / coverage / ci / review"| OK[recognized -> route]
    V -->|other| BAD[false -> reject]
    OK --> R[root agent routing]
    R -->|cron.daily| D1[summary digest workflow]
    R -->|lint| D2[lint-fixer workflow]
    R -->|coverage| D3[coverage-fixer workflow]
    R -->|ci| D4[resume lint/coverage fixer]
    R -->|review| D5[PR reviewer workflow]
```

Envelopes originate in the [webhook](/modules/platform/webhook.md) ingress and route
to the [summary](/modules/agents/summary.md),
[lint-fixer](/modules/agents/lintfixer.md), and
[coverage-fixer](/modules/agents/covfixer.md) workflows.

Adding a new ingress (e.g. Jira) means adding a `Kind` here (including its `Valid()` entry and the cross-port wire contract), a handler that emits
an `Envelope` — the root agent's routing is the only other place that changes.

## Wire codec

`Encode`/`Decode` are the envelope's JSON wire form, used when it crosses the Cloud Tasks
boundary ([tasks](/modules/platform/tasks.md) → `POST /internal/dispatch`). The form —
`kind`/`source` strings, `received_at` RFC 3339, `payload` standard base64 — is an
external contract and must stay byte-identical across all four language ports. `Decode`
rejects an unknown `Kind` as a permanent (poison) error so the worker acks rather than
retries it. The in-process transport passes the struct directly and never touches the
codec.
