# cmd

Executable entrypoints. Each subdirectory is a runnable module (a `__main__`-style
entrypoint).

## Flow

```mermaid
flowchart TD
    Build["python -m cmd.<name>"] --> Bins["one runnable entrypoint per subdir"]
    Bins --> Agent["cmd/agent (the automation-agent service)"]
    Agent --> Wire["wire automation_agent/ deps -> start long-running loops"]
    Wire --> NoLogic["no business logic here (lives in automation_agent/)"]
    NoLogic --> Rule["arch/test_nothing_imports_cmd:<br/>nothing else may import cmd/..."]
```

- `agent/` — the automation-agent service.

Entrypoints wire dependencies together and start long-running loops; they hold no
business logic (that lives in `automation_agent/`). Nothing else in the repo may import
`cmd/...` (enforced by `arch/`).
