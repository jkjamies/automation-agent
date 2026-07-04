# Tooling

* [CI gates](ci-gates.md) - Every port ships a full local CI gate (lint, typecheck/vet, architecture tests, tests, coverage ≥80%) that must pass before a change is proposed.
* [Specs & templates](specs-and-templates.md) - Non-trivial changes start as a spec generated from a template; specs are gitignored working memory, never referenced from code or standards.
* [Deployment topology](deployment.md) - The service deploys per-port on scale-to-zero Cloud Run behind a managed API gateway, with Cloud Scheduler cron, Cloud Tasks transport, Firestore state, and GitHub App auth.
