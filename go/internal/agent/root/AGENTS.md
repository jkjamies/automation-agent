# internal/agent/root

The dispatcher kicked off for every ingest. Build-agent pattern:

## Flow

```mermaid
flowchart TD
    Build["BuildRootDispatcher(Deps)"] -->|"SummaryDaily != nil"| RegC["Register KindCronDaily"]
    Build -->|"CoverageKickoff != nil"| RegCov["Register KindCoverage"]
    Build -->|"LintKickoff/CIResume != nil"| RegL["Register KindLint / KindCI"]
    Build -->|"ReviewKickoff != nil"| RegRev["Register KindReview"]
    RegC --> D["Dispatcher{handlers, log}"]
    RegCov --> D
    RegL --> D
    RegRev --> D
    GW["managed API gateway (single ingress)"] --> Ing["webhook HTTP server (/webhooks/*, /internal/*)"]
    Ing --> Env["ingest.Envelope"]
    Env --> Disp["Dispatch(ctx, env)"]
    Disp --> M{"handler for Kind?"}
    M -->|no| Warn["log warn + no-op (return nil)"]
    M -->|"cron.daily"| Sum["summaryHandler -> setup.Drive(summary runner)"]
    M -->|lint| LK["fixer.Kickoff(payload)"]
    M -->|coverage| CK["fixer.Kickoff(payload)"]
    M -->|ci| LR["fixer.Resume(payload)"]
    M -->|review| RK["reviewer.Kickoff(payload)"]
```

- `root.go` — `Dispatcher`: routes an `ingest.Envelope` to a `Handler` by `Kind`.
  Unregistered kinds are logged and ignored (so a not-yet-wired ingress is a no-op).
- `agents_setup.go` — `BuildRootDispatcher(Deps)` registers the available workflows when
  their deps are present: `KindCronDaily` → the summary workflow runner; `KindCoverage` →
  the coverage-fixer kickoff; `KindLint`/`KindCI` → the lint-fixer kickoff/resume;
  `KindReview` → the PR code-review agent kickoff (`ReviewKickoff`).

Keeping a single entry point is the point of "root": new ingress sources
(GitHub/Jira/Confluence/human) and smarter routing (e.g. LLM-based) slot in here
without restructuring. Today it is a deterministic dispatcher; it can become an ADK
agent when LLM routing is wanted.

Tested directly (routing, unhandled no-op, error propagation) plus a build test that
drives a real runner with a trivial stub agent — no LLM needed.
