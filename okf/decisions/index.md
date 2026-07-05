# Decisions

The load-bearing choices behind the current design, recorded so the *why* survives the
disposable specs where they were argued. Each record is factual and self-contained:
context → decision → consequences → alternatives considered. Lifecycle lives in
frontmatter (`status: accepted | superseded`), never in the body.

* [Two parity pairs](two-parity-pairs.md) - Why the four ports are organized as a modern Go/Python pair and a frozen Kotlin/TypeScript pair instead of four-way parity.
* [Cloud Tasks execution transport](cloud-tasks-transport.md) - Why webhook-triggered LLM compute re-enters the service through a Cloud Tasks queue and runs in-request on Cloud Run.
* [Firestore durable sessions on Cloud Run](firestore-durable-sessions.md) - Why parked runs persist in Firestore behind a session-backend switch rather than moving to Agent Runtime + Cloud SQL.
* [Gemini-on-Vertex model provider](gemini-vertex-model-provider.md) - Why production uses Gemma through the native Gemini client on Vertex, with Ollama as the local path and no proxy layer.
* [GitHub App auth & native triggers](github-app-auth-and-native-triggers.md) - Why production authenticates as a pinned-installation GitHub App, CI hooks in natively, and a generic gateway fronts the service.
* [The OKF knowledge bundle](okf-knowledge-bundle.md) - Why system knowledge consolidated into this bundle, replacing the per-directory AGENTS.md tree and .agents/standards.
