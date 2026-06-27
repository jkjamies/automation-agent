# src/agent/fixflow

The reusable, event-driven PR-fix engine shared by the lint-fixer and coverage-fixer.
A concrete agent supplies a `Spec` (triage + analyze functions, branch/label/check
names); the engine owns the loop, the apply mechanics, attempt counting, and the
suspend/resume bookkeeping against an injected `ParkStore` (memory by default, or a
durable sqlite/firestore backend per `SESSION_BACKEND`).

```mermaid
flowchart TD
    KO["Engine.kickoff(raw)"] --> PK["parseKickoff -> Kickoff"]
    PK --> DRV["Driver.kickoff: LongRunDriver.start"]
    DRV --> AF["apply_fix tool -> Engine.attemptOnce"]
    AF --> TR["spec.triage(llm, report) -> FileWork[]"]
    TR -->|"nothing actionable (NoWorkError)"| CLN["clean: notify cleanTitle, clear run; no PR, no park (stopWhen concludes)"]
    TR --> OR["openRepo (clone + checkout branch)"]
    OR --> AN["spec.analyze(input) -> FileEdit[]"]
    AN --> CM["commit: write edits -> commitAll -> push -> ensure labeled PR"]
    CM --> WAIT["await_ci (long-running) returns null -> PARK"]
    WAIT --> REG["ParkStore.park(prKey, record, ciTimeout, onTimeout)"]

    WH["Engine.resume(check_run webhook)"] --> RS{conclusion}
    RS -->|success| OK["notify success, clear run"]
    RS -->|"failure & attempts<maxIter"| RT["LongRunDriver.resume -> apply again -> re-park"]
    RS -->|"failure & attempts>=maxIter"| RV["notify needs-review, clear run"]
    TO["per-run timer fires"] -->|onTimeout| RV2["notify needs-review, free run"]
```

- `envelope.ts` — `Kickoff` / `parseKickoff`: the trusted kickoff envelope a CI job posts.
- `engine.ts` — `Engine`, `Spec`, `Deps`, `newEngine`: the loop owner + attempt logic.
- `driver.ts` — `Driver`: the CI-wait suspend/resume loop on long-running tools; owns all
  retry/stop/timeout policy and the per-session `RunParams` (never model-controlled).
  Persists each parked run through the injected `ParkStore`, whose single-winner claim
  (`resolveByPrKey` / `sweep`) is shared by the CI webhook, the soft timer, and the sweep.
- `applyfix.ts` — `openRepo` / `commit` / `applyFix`: clone, write edits (path-safe),
  commit, push, ensure a labeled PR.
- `analyze.ts` — `parallelAnalyze`: one analyzer agent per file, each writing distinct
  state keys, collecting non-empty edits.
- `explore.ts` — `explore`: a tool-using agent that reads the checkout to ground a plan.
- `tools.ts` / `files.ts` — read-only `read_file` / `list_dir` tools and the path-safe
  `safeJoin` that rejects absolute/escaping paths.
- `util.ts` — text-recovery helpers for model output (JSON extraction, fence stripping).

Durability comes from the injected `ParkStore` (`src/agent/setup/parkstore*.ts`): with the
sqlite or firestore backend parked runs survive a restart, and the periodic `/internal/sweep`
reconciles runs whose soft timer was lost. See `.agents/standards/architecture-design.md` §8.
