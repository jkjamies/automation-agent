# Workflow agents

* [Root dispatcher](root-dispatcher.md) - The single-entry-point dispatcher that routes every ingested event envelope to the workflow registered for its kind.
* [Daily summary](summary.md) - A scheduled workflow that fetches the last 24 hours of commits across repos in parallel, summarizes them with an LLM, and posts the digest to chat.
* [Fixflow engine](fixflow.md) - The reusable event-driven fix-loop engine behind the PR-fixing workflows — kickoff, apply, durable suspend across the CI wait, CI resume, loop or finish.
* [Lint fixer](lintfixer.md) - The autonomous lint-remediation workflow — a lint-specific configuration of the shared fixflow engine that triages a lint report, pushes a fix PR, and loops on CI feedback.
* [Coverage fixer](covfixer.md) - The test-coverage configuration of the fixflow engine — triages a coverage report, plans test placement by exploring the repo's real conventions, and generates tests on a CI-verified loop.
* [PR reviewer](reviewer.md) - The in-house PR code-review workflow that reacts to pull_request events and posts an advisory review — per-category findings, a count-based scorecard, inline suggestions, and a never-blocking check.
* [Setup](setup.md) - The shared agent-building utilities and the only package allowed to import provider SDKs — owning the LLM provider switch, the durable session backend, and the ParkStore.
