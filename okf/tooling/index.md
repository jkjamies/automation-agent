# Tooling

* [Agent skills](skills.md) - Reusable agent procedures under .agents/skills/ that chain requirements → spec → grilled decisions → implementation → verification → knowledge updates, each citing this bundle for house rules rather than restating them.
* [CI gates](ci-gates.md) - The full local CI gate (lint, vet, architecture tests, tests, coverage ≥80%) that must pass before a change is proposed.
* [Specs & templates](specs-and-templates.md) - Non-trivial changes start as a spec generated from a template; specs are gitignored working memory, never referenced from code or standards.
* [Deployment topology](deployment.md) - The service deploys per-port on scale-to-zero Cloud Run behind a managed API gateway, with Cloud Scheduler cron, Cloud Tasks transport, Firestore state, and GitHub App auth.
