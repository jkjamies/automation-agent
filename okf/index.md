---
okf_version: "0.2"
bundle: automation-agent
---

# automation-agent — knowledge bundle

The canonical knowledge for `automation-agent`: an event-driven automation service
(daily summaries, autonomous lint/coverage fixers with a PR + CI loop, and a PR
reviewer) built on the Agent Development Kit. New here? Start with
[what this system is](orientation/what-this-system-is.md).

# Orientation

* [orientation](orientation/index.md) - What the system is, how events flow, the durable suspend/resume design, and the glossary.

# Standards

* [standards](standards/index.md) - The rules every change obeys: architecture boundaries, testing, style, documentation, security, webhooks, observability, CI integration, deployment, and local development.

# Modules

* [modules](modules/index.md) - The system's units: workflow agents and deterministic platform packages.

# Tooling

* [tooling](tooling/index.md) - CI gates, the spec/template process, and the deployment topology.
