# src/agent/setup

Shared utilities for building agents. **This is the only module allowed to import
provider SDKs** (the Ollama adapter / Gemini / genai) — enforced by `arch/`.

```mermaid
flowchart TD
    Cfg["config.Config"] --> BL["buildLLM(cfg)"]
    BL -->|ollama| OM["new OllamaLlm(host, model) -> BaseLlm"]
    BL -->|gemini| GM["new Gemini({model}) -> BaseLlm"]
    OM -->|"forwards tools + config via fetch /api/chat"| Oll[("Ollama / Gemma")]
    GM --> Vtx[("Gemini / Vertex")]
    Agents["root / summary / lintfixer / covfixer"] -->|"BaseLlm, generate"| OM
    Agents --> GM
    Prompts["Prompts (fs): get / mustGet"] --> Agents
    Runner["newRunner + drive / driveText / driveCollectState"] --> Agents
    Events["userText / assistantText / textEvent / stateString"] --> Agents
    LongRun["LongRunDriver + Sequencer (suspend/resume)"] --> Agents
```

- `llm.ts` — `buildLLM(cfg)` / `buildCodeLLM(cfg)`: the provider switch returning a
  `BaseLlm`. Provider selection lives entirely here.
- `ollama.ts` — `OllamaLlm extends BaseLlm`: the Ollama adapter. adk-js ships no Ollama
  model, so this forwards tool declarations + generation config to a local `/api/chat`
  endpoint over `fetch`.
- `gemini.ts` — `newGeminiModel(model)`: the cloud path via ADK's `Gemini`.
- `prompt`/`prompts.ts` — `Prompts`, a markdown loader (each agent ships its own
  `prompts/` dir, read from disk relative to `import.meta.url`).
- `events.ts` — small genai content helpers (`userText`, `contentText`, `lastText`,
  `textEvent`, `stateString`).
- `runner.ts` — in-memory runner helpers (`newRunner`, `drive`, `driveText`,
  `driveCollectState`).
- `longrun.ts` — generic ADK **long-running** suspend/resume plumbing: `LongRunDriver`
  (`start`/`resume` returning a plain `DriveResult`) and the `Sequencer` class, a
  deterministic Action->Wait `BaseLlm` for two-phase wait loops. Lives here because it
  touches `genai`; callers (e.g. `fixflow`) stay genai-free.
- `generate.ts` — text-generation helpers over the configured `BaseLlm`.

Tests stub the Ollama HTTP endpoint (`fetch`) and use a temp dir for prompts — no real
network, no live model. Never assert on LLM output content.
