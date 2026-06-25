# automation_agent/agent/root

The dispatcher kicked off for every ingest. Build-agent pattern:

## Flow

```mermaid
flowchart TD
    Build["build_root_dispatcher(Deps)"] -->|"summary_daily is not None"| RegC["register KindCronDaily"]
    Build -->|"coverage_kickoff is not None"| RegCov["register KindCoverage"]
    Build -->|"lint_kickoff/ci_resume is not None"| RegL["register KindLint / KindCI"]
    RegC --> D["Dispatcher{handlers, log}"]
    RegCov --> D
    RegL --> D
    GW["managed API gateway (single ingress)"] --> Ing["webhook HTTP server (/webhooks/*, /internal/*)"]
    Ing --> Env["ingest.Envelope"]
    Env --> Disp["dispatch(env)"]
    Disp --> M{"handler for Kind?"}
    M -->|no| Warn["log warn + no-op (return None)"]
    M -->|"cron.daily"| Sum["summary_handler -> setup.drive(summary runner)"]
    M -->|lint| LK["fixer.kickoff(payload)"]
    M -->|coverage| CK["fixer.kickoff(payload)"]
    M -->|ci| LR["fixer.resume(payload)"]
```

- `root.py` — `Dispatcher`: routes an `ingest.Envelope` to a `Handler` by `Kind`.
  Unregistered kinds are logged and ignored (so a not-yet-wired ingress is a no-op).
- `agents_setup.py` — `build_root_dispatcher(Deps)` registers the available workflows when
  their deps are present: `KindCronDaily` -> the summary workflow runner; `KindCoverage` ->
  the coverage-fixer kickoff; `KindLint`/`KindCI` -> the lint-fixer kickoff/resume.

Keeping a single entry point is the point of "root": new ingress sources
(GitHub/Jira/Confluence/human) and smarter routing (e.g. LLM-based) slot in here
without restructuring. Today it is a deterministic dispatcher; it can become an ADK
agent when LLM routing is wanted.

Tested directly (routing, unhandled no-op, error propagation) plus a build test that
drives a real runner with a trivial stub agent — no LLM needed.
