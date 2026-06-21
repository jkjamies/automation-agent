# cmd

Executable entrypoints. Each subdirectory is a runnable module (`tsx cmd/<name>/main.ts`).

```mermaid
flowchart TD
    Run["tsx cmd/<name>/main.ts"] --> Bins["one runnable entrypoint per subdir"]
    Bins --> Agent["cmd/agent (the automation-agent service)"]
    Agent --> Wire["wire src/ deps -> start long-running loops"]
    Wire --> NoLogic["no business logic here (lives in src/)"]
    NoLogic --> Rule["arch test: nothing else may import cmd/..."]
```

- `agent/` — the automation-agent service.
- `playground/` — a local dev REPL over the configured model (never deployed).

Entrypoints wire dependencies together and start long-running loops; they hold no
business logic (that lives in `src/`). Nothing else in the repo may import `cmd/...`
(enforced by `arch/`).
