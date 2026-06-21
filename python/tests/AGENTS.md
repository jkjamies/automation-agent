# tests

The pytest unit-test suite. One test module per package: `tests/test_<pkg>.py`
covers `automation_agent/<pkg>` (e.g. `tests/test_config.py` covers `config`,
`tests/test_engine.py` covers the fixflow engine).

## Tooling

- **pytest** — the runner.
- **respx** — intercepts outbound HTTP (GitHub REST API, Slack/Teams webhooks, the
  Ollama endpoint) so no test touches the network.
- **FastAPI `TestClient`** — drives the webhook endpoints in-process.
- **Fake / scripted LLMs** — a stub `BaseLlm` (and the deterministic sequencer model)
  stands in for Ollama/Gemini. Live-model tests are gated behind `OLLAMA_LIVE`.
- Local seed git repos exercise `gitrepo`/`applyfix` without a remote.

## Rules

- **Never assert on LLM output content.** Tests assert on structure, state keys,
  routing, control flow, parsing, and side effects — not on what the model wrote.
- Provider SDKs are confined to `automation_agent/agent/setup` (enforced by `arch/`),
  so everything else is fakeable.
- **Coverage gate >=80%** over `automation_agent/` (`cmd/` is composition-only and
  excluded), enforced via `make cover`.

Architecture-conformance tests live separately under `arch/` (run with `make arch`).
