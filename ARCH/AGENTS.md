# ARCH

Architecture-conformance tests. Pure standard-library (no external deps) so they
run anywhere. Run with `make arch`.

## Flow

```mermaid
flowchart TD
    Make["make arch"] --> Run["go test ./ARCH"]
    Run --> Helpers["repoRoot() = abs('..')<br/>modulePath() reads go.mod"]
    Helpers --> Walk["goFiles(root): WalkDir + go/parser<br/>ParseFile(ImportsOnly)"]
    Walk -->|"skipDir: .git .claude node_modules vendor specs"| FI["[]fileImports{path, imports}"]

    FI --> T1["TestToolingDoesNotImportAgents"]
    FI --> T2["TestProviderSDKsOnlyInSetup"]
    FI --> T3["TestNothingImportsCmd"]
    Root["WalkDir(root) for AGENTS.md"] --> T4["TestEveryDirHasAgentsDoc"]

    T1 --> C1{"under internal/{githubapi,gitrepo,webhook,<br/>notify,scheduler} imports internal/agent?"}
    C1 -->|yes| F1["t.Errorf: tooling must not depend on agents"]
    C1 -->|no| OK1["pass"]

    T2 --> C2{"imports ollama/ollama, adk/model/gemini,<br/>google.golang.org/genai outside agent/setup?"}
    C2 -->|yes| F2["t.Errorf: provider SDK outside setup"]
    C2 -->|no| OK2["pass"]

    T3 --> C3{"any file (not under cmd/) imports cmd/...?"}
    C3 -->|yes| F3["t.Errorf: imports cmd package"]
    C3 -->|no| OK3["pass"]

    T4 --> C4{"every dir has AGENTS.md?<br/>(skipDocDir: docs, prompts, models,<br/>tasks, testdata, hidden != .agents)"}
    C4 -->|missing| F4["t.Errorf: missing AGENTS.md in <dir>"]
    C4 -->|"all present (.agents not descended)"| OK4["pass"]
```

Current rules:

- `TestToolingDoesNotImportAgents` — `internal/{githubapi,gitrepo,webhook,notify,
  scheduler}` must not import `internal/agent/...`.
- `TestProviderSDKsOnlyInSetup` — Ollama/Gemini/genai imports are confined to
  `internal/agent/setup`.
- `TestNothingImportsCmd` — no package imports `cmd/...`.
- `TestEveryDirHasAgentsDoc` — every non-exempt directory has an `AGENTS.md`.

Add a new test here whenever we introduce a structural rule worth protecting.
