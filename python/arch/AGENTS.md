# arch

Architecture-conformance tests. Pure standard-library (the `ast` module, no external
deps beyond pytest) so they run anywhere. Run with `make arch`.

## Flow

```mermaid
flowchart TD
    Make["make arch"] --> Run["pytest arch/"]
    Run --> Helpers["repo_root() = python/ dir<br/>conftest helpers"]
    Helpers --> Walk["python_files(root): walk + ast.parse<br/>collect import statements"]
    Walk -->|"skip: .venv __pycache__ caches specs"| FI["[fileImports{path, imports}]"]

    FI --> T1["test_tooling_does_not_import_agents"]
    FI --> T2["test_provider_sdks_only_in_setup"]
    FI --> T3["test_nothing_imports_cmd"]
    Root["os.walk(root) for AGENTS.md"] --> T4["test_every_dir_has_agents_doc"]

    T1 --> C1{"under automation_agent/{githubapi,gitrepo,webhook,<br/>notify,scheduler} imports automation_agent.agent?"}
    C1 -->|yes| F1["assert fails: tooling must not depend on agents"]
    C1 -->|no| OK1["pass"]

    T2 --> C2{"imports litellm/lite_llm,<br/>google.adk.models.Gemini, google.genai outside agent/setup?"}
    C2 -->|yes| F2["assert fails: provider SDK outside setup"]
    C2 -->|no| OK2["pass"]

    T3 --> C3{"any file (not under cmd/) imports cmd/...?"}
    C3 -->|yes| F3["assert fails: imports cmd package"]
    C3 -->|no| OK3["pass"]

    T4 --> C4{"every dir has AGENTS.md?<br/>(skip: docs, prompts, models,<br/>tasks, testdata, hidden != .agents, build artifacts)"}
    C4 -->|missing| F4["assert fails: missing AGENTS.md in <dir>"]
    C4 -->|"all present (.agents not descended)"| OK4["pass"]
```

Current rules:

- `test_tooling_does_not_import_agents` (`arch/test_arch.py`) —
  `automation_agent/{githubapi,gitrepo,webhook,notify,scheduler}` must not
  import `automation_agent.agent...`.
- `test_provider_sdks_only_in_setup` (`arch/test_arch.py`) — `litellm`/`lite_llm`/
  `google.adk.models.Gemini`/`google.genai` imports are confined to
  `automation_agent/agent/setup`.
- `test_nothing_imports_cmd` (`arch/test_arch.py`) — no package imports `cmd/...`.
- `test_every_dir_has_agents_doc` (`arch/test_docs.py`) — every non-exempt directory
  has an `AGENTS.md`.

Add a new test here whenever we introduce a structural rule worth protecting.
