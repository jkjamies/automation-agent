# automation_agent/agent/lintfixer

The autonomous lint-remediation workflow. It is a configuration of the shared
`fixflow` engine: its own triage/analyze functions and prompts, on `fixflow`'s
deterministic kickoff -> suspend -> CI resume -> loop or finish loop. The wait is a real
ADK long-running suspend/resume because CI takes 20–40 min. The ADK session and the parked
run are persisted through `SESSION_BACKEND` (`memory` | `sqlite` | `firestore`) via the
injected `setup.ParkStore` (a `ParkRecord` keyed by a UUID session id, with an
`owner/repo#pr` `pr_key` index for CI resume). With a durable backend a process restart
resumes in-flight runs; the default `memory` backend stays ephemeral. Each wait is freed two
ways: a soft per-run `CI_TIMEOUT` timer and the durable `/internal/sweep` catch-all.

## Flow

```mermaid
flowchart TD
    K["KindLint -> kickoff(raw)"] --> KP["parse_kickoff{repo, base, report}"]
    KP --> T["triage(LLM): report -> [FileProblems]"]
    T --> FF["gh.get_file_content per file (base)"]
    FF --> AN["run_analyze: ParallelAgent[analyze_<file>] -> [FileEdit]"]
    AN --> AF["apply_fix: clone -> new branch -> commit -> push -> create_pr + label"]
    AF --> SUS(["suspend: PR open, await CI (durable: survives restart)"])

    SUS -->|"agent-lint-verify check_run"| CI["KindCI -> resume(raw)"]
    TO["CI_TIMEOUT timer"] -.->|"CI never reports"| HR
    SW["/internal/sweep (durable catch-all)"] -.->|"CI never reports"| HR
    CI --> RH["Engine.resume(ResumeInput)"]
    RH --> C{conclusion}
    C -->|success| OK["notify success + PR link"]
    C -->|failure| AT{"park-record attempts >= max_iter?"}
    AT -->|yes| HR["notify needs human review + PR link"]
    AT -->|no| RT["attempt(retry): re-triage from check output, read branch files, analyze w/ feedback, apply_fix(new_branch=False)"]
    RT --> SUS
    OK --> Chat[("Slack / Teams")]
    HR --> Chat
```

- **Kickoff** (`KindLint`) -> `Fixer.kickoff`: parse the trusted `{repo, base, report}`
  envelope -> `triage` (LLM normalizes the arbitrary report) -> fetch file contents ->
  `run_analyze` (one parallel agent per file) -> `apply_fix` (branch, commit, push,
  labeled PR) -> suspend.
- **Resume** (`KindCI`) -> `Engine.resume` (the `fixflow` Driver): on the agent verify
  check completing — success -> notify; failure & attempts < max -> re-analyze with CI
  feedback and push onto the same branch; failure & attempts >= max -> notify "needs
  human review" + PR link. Attempts are counted in the `setup.ParkStore` park record, not
  from GitHub commits. A parked run whose CI never reports is freed two ways: a per-run
  `CI_TIMEOUT` timer and the durable `/internal/sweep` catch-all (both -> "needs human
  review").

## Files

- `lint.py` — `new_engine(Deps)`: the lint `Spec` (branch/label/check + titles) that
  configures the shared `fixflow` engine.
- `triage.py` — LLM report normalization (format-agnostic; live-proven).
- `analyze.py` — parallel per-file fix agents (live-proven).
- `loader.py` — prompt loading over this dir's `prompts/`.
- `prompts/{triage,analyze,summarize_result}.md`.

Wiring: `root` registers `KindLint`/`KindCI`; `cmd` builds the engine (via
`new_engine`) and the webhook server. The kickoff/suspend/resume
mechanics live in `fixflow`. Provider SDKs (genai) are kept out via `setup` helpers.
Tests use a stub/scripted LLM + fakes + a local seed repo; live LLM tests are gated
behind `OLLAMA_LIVE`. See `.agents/standards/architecture-design.md` §8.
