# Standards

The rules every change obeys. The architecture & build plan is the authoritative design;
the rest are focused contracts a change must not break.

* [Architecture & build plan](architecture-design.md) - The authoritative language-neutral design of the automation agent — goals, topology, durable suspend/resume sessions, configuration, and deployment.
* [Architecture rules](architecture.md) - The import-boundary and durable-session state rules the architecture test suite enforces identically across every port.
* [Language parity](language-parity.md) - The cross-language parity contract organizing the four ports into a modern Go/Python pair and a frozen Kotlin/TypeScript pair.
* [The build-agent pattern](agent-build-pattern.md) - Splits every standalone agent directory into pure ADK wiring and deterministic LLM-free logic so structure and behavior are testable without a model.
* [Testing](testing.md) - How to run every kind of test for each port and the rules they obey — 80% coverage, no LLM-content assertions, and stubbed networks.
* [Go style](go-style.md) - Idiomatic Go conventions for the reference port — formatting, errors, naming, dependency injection, configuration, and context.
* [Documentation & diagrams](documentation.md) - How the repo documents itself — the knowledge bundle, factual docs, and the rule that docs and diagrams move with the code in the same change.
* [OKF format](okf-format.md) - The bundle's own format contract — layout, frontmatter schema, the type taxonomy, timestamp semantics, link convention, and the conformance floor.
* [Security model](security.md) - The service's security controls in one view — ingress authentication per route class, credential lifecycle, secret storage, and input hardening.
* [Webhooks & CI check names](webhooks.md) - The canonical registry of the agent's webhook routes and GitHub check_run names, which every port must match as an external contract.
* [Observability](observability.md) - The distributed-tracing design — one globally registered tracer provider the agent framework inherits, four config-selected exporters, backend-aware propagation, and an in-request flush.
* [CI integration](ci-integration.md) - How a CI pipeline on any tech stack kicks off the lint and coverage fixers and wires the verify checks that resume them.
* [Deployment](deployment.md) - The canonical deployment and operations reference — HTTP surface, execution transport, GitHub App auth, step-by-step GCP setup, and the private-ingress architecture.
* [Local development](local-development.md) - How to run the service locally in every mode — prerequisites, run targets, backend switches, and the full environment-variable reference.
