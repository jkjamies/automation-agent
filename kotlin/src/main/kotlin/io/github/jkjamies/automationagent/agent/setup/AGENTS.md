# agent.setup

Shared utilities for building agents.
**This is the only package allowed to import provider SDKs** (the Ollama HTTP client,
ADK-Kotlin's `Gemini` model, and the `com.google.adk.kt.models`/`com.google.genai`
types) — enforced by the `:konsist` `ArchitectureTest`.

## Flow

```mermaid
flowchart TD
    Cfg["config.Config"] --> BL["buildLLM(cfg)"]
    BL -->|ollama| OM["OllamaModel (Model)"]
    BL -->|gemini| GM["Gemini (Model)"]
    OM -->|"/api/chat: Content <-> Ollama messages, stream aggregate"| Oll[("Ollama / Gemma")]
    GM --> Vtx[("Gemini / Vertex")]
    Agents["root / summary / lintfixer"] -->|"Model, generateText"| OM
    Prompts["Prompts.forAgent(name).get(...)"] --> Agents
    Runner["newRunner + drive / driveText / driveCollectState"] --> Agents
    Long["LongRunDriver + newSequencerModel"] --> Agents
    Events["userText / contentText / textEvent / stateString"] --> Agents
```

- `Llm.kt` — `buildLLM(cfg)` / `buildCodeLLM(cfg)`: the provider switch returning a `Model`,
  plus `newGeminiModel`. No `context` argument — ADK-Kotlin is coroutine-based.
- `OllamaModel.kt` — the `Model` adapter over a local Ollama server. ADK-Kotlin
  ships no Ollama model, and there is no official Kotlin client, so the `/api/chat` round-trip
  is implemented directly over **Ktor** (already a dependency). Converts genai content ⇄ Ollama
  chat messages and aggregates streaming chunks. The `HttpClient` is injectable for tests.
- `Generate.kt` — `generateText(llm, system, user)`: a single non-streaming completion.
- `Prompts.kt` — a markdown loader over the classpath (each agent keeps `resources/prompts/<agent>/`).
- `Events.kt` — content/event helpers (`userText`, `assistantText`, `contentText`, `lastText`,
  `textEvent`, `stateString`).
- `Runner.kt` — in-memory runner helpers (`newRunner`, `drive`, `driveText`, `driveCollectState`).
- `Longrun.kt` — generic suspend/resume plumbing: `LongRunDriver` (built on ADK-Kotlin
  **resumability**, `ResumabilityConfig(isResumable = true)` for long-running
  flows) and `newSequencerModel`, a deterministic action→wait `Model` for two-phase
  wait loops. Lives here because it touches the genai types; callers (e.g. `fixflow`) stay
  genai-free.

Notable idiomatic choices: `Flow`/coroutines drive iteration; errors throw from flows rather than
being yielded; `DriveResult.parkedCallId` is a nullable `String?`; ADK-Kotlin's config carries no
`seed`, so the Ollama options omit it.

Tests stub the Ollama server with a Ktor `MockEngine` and drive the long-running loop with
hand-rolled `BaseTool`s — no real network, no live model. Never assert on LLM output content.
