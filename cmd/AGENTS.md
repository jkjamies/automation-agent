# cmd

Executable entrypoints. Each subdirectory is a `package main` binary.

## Flow

```mermaid
flowchart TD
    Build["go build ./cmd/..."] --> Bins["one package main binary per subdir"]
    Bins --> Agent["cmd/agent (the automation-agent service)"]
    Agent --> Wire["wire internal/ deps -> start long-running loops"]
    Wire --> NoLogic["no business logic here (lives in internal/)"]
    NoLogic --> Rule["ARCH/TestNothingImportsCmd:<br/>nothing else may import cmd/..."]
```

- `agent/` — the automation-agent service.

Entrypoints wire dependencies together and start long-running loops; they hold no
business logic (that lives in `internal/`). Nothing else in the repo may import
`cmd/...` (enforced by `ARCH/`).
