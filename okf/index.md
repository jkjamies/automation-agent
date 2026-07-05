# automation-agent — knowledge bundle

The canonical knowledge for `automation-agent`: an event-driven automation service
(daily summaries, autonomous lint/coverage fixers with a PR + CI loop, and a PR
reviewer) built on the Agent Development Kit and maintained as parallel language ports
of one design. New here? Start with
[what this system is](orientation/what-this-system-is.md).

# Orientation

* [orientation](orientation/index.md) - What the system is, how events flow, the durable suspend/resume design, and the glossary.

# Standards

* [standards](standards/index.md) - The rules every change obeys: architecture boundaries, language parity, testing, style, documentation, security, webhooks, observability, CI integration, deployment, and local development.

# Decisions

* [decisions](decisions/index.md) - Why the load-bearing choices were made — parity pairs, execution transport, durable sessions, model provider, GitHub App auth, and the knowledge bundle itself.

# Modules

* [modules](modules/index.md) - The system's units: workflow agents, deterministic platform packages, and the four language ports.

# Tooling

* [tooling](tooling/index.md) - CI gates, the spec/template process, and the deployment topology.
