# cmd/playground/chat

The ADK agent package the playground serves. `adk web cmd/playground` discovers this
subdirectory and loads its `root_agent` (a chat `LlmAgent` over the configured model).

- `agent.py` — builds `root_agent` via `setup.build_llm(config.load())`. Development
  only; swap in the summary / lintfixer / covfixer agents here to drive the real
  workflows interactively.
- `__init__.py` — re-exports `root_agent` for the ADK loader.

Not part of the production image (which runs only `cmd/agent`).
