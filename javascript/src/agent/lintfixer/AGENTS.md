# src/agent/lintfixer

The autonomous lint-remediation workflow. It is a configuration of the shared `fixflow`
engine: its own triage/analyze functions and prompts, on `fixflow`'s deterministic
kickoff -> suspend -> CI resume -> loop or finish loop. The wait is a real long-running
suspend/resume because CI takes 20–40 min. The parked run is persisted through `fixflow`'s
injected `ParkStore` (indexed by `owner/repo#pr`), whose backend is selected by
`SESSION_BACKEND` — with sqlite/firestore it survives a restart, and `/internal/sweep`
reconciles a lost timer. A per-run `CI_TIMEOUT` timer bounds each wait.

```mermaid
flowchart TD
    K["Lint -> kickoff(raw)"] --> KP["parseKickoff{repo, base, report}"]
    KP --> T["triage(LLM): report -> FileWork[]"]
    T --> AF["apply_fix: clone -> branch -> analyze (ParallelAgent per file) -> commit -> push -> create + label PR"]
    AF --> SUS(["suspend: PR open, await CI"])
    SUS -->|"agent-lint-verify check_run"| CI["CI -> resume(raw)"]
    TO["CI_TIMEOUT timer (in-memory)"] -.->|"CI never reports"| HR
    CI --> C{conclusion}
    C -->|success| OK["notify success + PR link"]
    C -->|"failure & attempts<maxIter"| RT["re-analyze with CI feedback, push to same branch"]
    C -->|"failure & attempts>=maxIter"| HR["notify needs human review + PR link"]
    RT --> SUS
    OK --> Chat[("Slack / Teams")]
    HR --> Chat
```

## Files

- `lint.ts` — `newLintEngine(Deps)`: the lint `Spec` (branch/label/check + titles) that
  configures the shared `fixflow` engine.
- `triage.ts` — LLM report normalization (format-agnostic).
- `analyze.ts` — parallel per-file fix agents.
- `loader.ts` — prompt loading over this dir's `prompts/`.
- `prompts/{triage,analyze,summarize_result}.md`.

Wiring: `root` registers `Lint`/`CI`; `cmd` builds the engine (via `newLintEngine`) and
the webhook server. The kickoff/suspend/resume mechanics live in `fixflow`.
Provider SDKs are kept out via `setup` helpers. Tests use a scripted LLM + fakes + a local
seed repo. See `.agents/standards/architecture-design.md` §8.
