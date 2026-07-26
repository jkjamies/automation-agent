---
type: Architecture
title: What this system is
description: An event-driven automation service that summarizes repo activity, autonomously fixes lint/coverage failures via PR + CI loops, and reviews pull requests — implemented as parallel language ports of one design.
tags: [orientation, overview]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

`automation-agent` is a single long-running service built on the Agent Development Kit
(ADK). It ingests events from many sources (scheduled cron, GitHub webhooks, CI report
webhooks), normalizes every event into one envelope, routes it through a root dispatcher,
and runs one of four workflow agents:

- **[Summary](/modules/agents/summary.md)** — a daily digest of commit activity across
  configured repos, posted to Slack or Teams.
- **[Lint-fixer](/modules/agents/lintfixer.md)** — receives a lint report, autonomously
  edits the offending repo, opens a PR, and loops on the CI verdict (up to a retry cap).
- **[Coverage-fixer](/modules/agents/covfixer.md)** — the same PR + CI loop applied to
  test-coverage remediation. Both fixers are thin specs over the shared
  [fixflow engine](/modules/agents/fixflow.md).
- **[Reviewer](/modules/agents/reviewer.md)** — an in-house PR code reviewer: one-shot,
  advisory, comment-only, standards-aware.

Everything enters through the **[root dispatcher](/modules/agents/root-dispatcher.md)**:
one entry point, routed by event `Kind`. Deterministic tooling
([githubapi](/modules/platform/githubapi.md), [gitrepo](/modules/platform/gitrepo.md),
[webhook](/modules/platform/webhook.md), [notify](/modules/platform/notify.md)) is
agent-free — agents call it; it never imports agents. That boundary is enforced by
architecture tests.

## The defining design fact

**The CI wait is durable and infrastructure-owned.** A fix cannot be confirmed for 20–40+
minutes while CI runs, and the service deploys on scale-to-zero Cloud Run — so a fix run
*parks* (a durable workflow pause persisted in the session store) and *resumes* when a
GitHub `check_run` webhook reports back. No in-process wait, no resident worker. See
[suspend/resume design](/orientation/suspend-resume-design.md).

## Model strategy

Local-first on Ollama + Gemma (a 26 B model for code reasoning, a 12 B model for
summarization), with a clean switch to Gemini/Vertex for the persistent GCP deployment —
both behind one model-builder seam confined to the
[setup package](/modules/agents/setup.md). Agents never hardcode a provider.

## Where to go next

- The path an event takes: [event flow](/orientation/event-flow.md)
- The park/resume spine: [suspend/resume design](/orientation/suspend-resume-design.md)
- The authoritative design document: [architecture & build plan](/standards/architecture-design.md)
- Rules every change must obey: [standards index](/standards/index.md)
