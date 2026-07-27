# Platform packages

Deterministic and agent-free: agents call these; they never import agents (enforced by
the architecture tests).

* [Runtime configuration](config.md) - Single source of truth for runtime configuration, loaded once from the environment, validated at startup, and passed down so no other package reads env vars.
* [GitHub authentication](auth.md) - Single TokenProvider seam that hides whether GitHub credentials come from a static PAT or a freshly minted GitHub App installation token.
* [GitHub REST API client](githubapi.md) - Thin GitHub REST client exposing only what the service needs — commit digests, PR create/find, labels, check-run status, and file content.
* [Git working-tree operations](gitrepo.md) - Clone, branch, commit, and push working-tree git operations with scheme-driven auth and per-operation token refresh.
* [Ingress envelope](ingest.md) - The normalized Envelope every ingress source reduces to before reaching the root agent, plus the JSON wire codec that carries it across the Cloud Tasks boundary.
* [HTTP ingress](webhook.md) - The HTTP ingress — HMAC-verified /webhooks/* endpoints, bearer-gated /internal/* Cloud Scheduler triggers, and the Cloud Tasks dispatch worker.
* [Execution transport](tasks.md) - The Enqueue transport between webhook ingress and the dispatcher, with in-process and Cloud Tasks backends so long LLM compute runs in-request on Cloud Run.
* [Notifications](notify.md) - Provider-agnostic notifier that posts Messages to Slack or Microsoft Teams behind one interface, chosen by a config flag.
* [Observability](obs.md) - Tracer-provider registration, trace propagation across the Cloud Tasks boundary, and scale-to-zero-safe flushing for the agent framework's native spans.
