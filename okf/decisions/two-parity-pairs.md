---
type: Decision
title: Two parity pairs instead of four-way parity
description: Why the four ports are organized as a modern Go/Python pair and a frozen Kotlin/TypeScript pair, with no parity requirement across the pairs.
tags: [decision, parity, ports]
sensitivity: internal
bundle: automation-agent
status: accepted
decided: 2026-07-03
timestamp: 2026-07-04T00:00:00Z
---

# Two parity pairs instead of four-way parity

## Context

The service is maintained as four language ports of one design, each on its language's
native ADK. The ADKs diverged: the Go and Python ADKs carry a 2.x line (graph workflows,
request-input pause/resume), while the Kotlin and JavaScript ADKs do not. Keeping all
four ports behavior-identical meant every design improvement was capped by the least
capable SDK, and every change cost four implementations plus four review passes.

## Decision

Reorganize parity into **two pairs with no parity requirement across them**:

- **Modern pair — Go ↔ Python**: carries the design forward on the ADK 2.x line under
  the full parity contract; Go is the reference and changes land there first.
- **Frozen pair — Kotlin ↔ TypeScript**: feature-frozen at its current behavior, kept
  1:1 with *each other*; a critical fix touching one lands in both.
- **External contracts** (webhook routes, `check_run` names, notify payloads) still
  match across all four ports, because outside systems observe them.

The full contract is [Language parity](/standards/language-parity.md).

## Consequences

- Feature work costs two ports, not four, and may use ADK 2.x capabilities freely
  (e.g. the fixers' CI wait is a workflow-graph pause on the modern pair and
  long-running tools on the frozen pair).
- The frozen pair's mechanics and the design document drift apart deliberately; the
  design document describes the modern pair.
- The frozen ports remain deployable and demonstrate the same external contract on a
  second SDK generation.

## Alternatives considered

- **Keep four-way parity** — rejected: it pins the design to the lowest-common-
  denominator SDK and quadruples the cost of every change.
- **Delete the Kotlin/TypeScript ports** — rejected: they are complete, tested, and
  still prove the design is language-neutral; freezing costs almost nothing.
