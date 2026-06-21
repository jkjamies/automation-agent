# tests

The pytest unit-test suite. It **mirrors the Go `*_test.go` files one-to-one**: each
Go `<pkg>_test.go` maps to `tests/test_<pkg>.py` (e.g. `config_test.go` ->
`tests/test_config.py`, `engine_test.go` -> `tests/test_engine.py`,
`applyfix_test.go` -> `tests/test_applyfix.py`). Keeping the mapping one-to-one is
part of the parity contract — when a Go test changes, its Python twin changes in the
same logical change.

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
