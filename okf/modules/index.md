# Modules

The system's units, documented once for all ports (the Go port is the reference
implementation; per-port deltas live in the port concepts).

# Subdirectories

* [agents](agents/index.md) - The workflow agents: the root dispatcher, the daily summary, the two fixers over the shared fixflow engine, the PR reviewer, and the setup layer they are built on.
* [platform](platform/index.md) - The deterministic, agent-free platform packages: config, auth, GitHub API, git working tree, ingress envelope, webhooks, notifications, observability, and the execution transport.
* [ports](ports/index.md) - The four language ports and how the two parity pairs relate.
