# Modules

The system's units: the service itself, its workflow agents, and its deterministic platform packages.

# Subdirectories

* [agents](agents/index.md) - The workflow agents: the root dispatcher, the daily summary, the two fixers over the shared fixflow engine, the PR reviewer, and the setup layer they are built on.
* [platform](platform/index.md) - The deterministic, agent-free platform packages: config, auth, GitHub API, git working tree, ingress envelope, webhooks, notifications, observability, and the execution transport.

# Concepts

* [The service](service.md) - The Go service under `go/` — its kickoff and resume flows, package layout, build targets, and the conventions the architecture tests enforce.
