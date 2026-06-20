# internal/reconcile

The stateless recovery scan — GitHub is the source of truth, so there is no local
store. On startup and on a timer (`RECONCILE_INTERVAL`), `Scan` lists labeled PRs
per repo and reads each one's agent verify check, classifying each into:

## Flow

```mermaid
flowchart TD
    Timer["cmd/agent: runReconcileLoop<br/>(startup + RECONCILE_INTERVAL ticker)"] --> Scan["Scan(ctx)"]
    Scan --> RepoLoop{"for repo in cfg.Repos"}
    RepoLoop --> Split["splitRepo('owner/repo')"]
    Split -->|invalid| ErrR["collect err, continue"]
    Split -->|ok| Find["gh.FindAgentPRs(owner, repo, cfg.Label)"]
    Find -->|err| ErrR
    Find -->|"[]githubapi.PR"| PRLoop{"for pr in prs"}
    PRLoop --> Handle["handlePR -> gh.AgentCheck(owner, repo, pr.HeadSHA, cfg.CheckName)"]
    Handle -->|err| Pend0["Action{Outcome: pending} + collect err"]
    Handle -->|"githubapi.CheckResult"| Classify["classify(check)"]

    Classify --> CFound{"check.Found?"}
    CFound -->|no| NoCheck["OutcomeNoCheck"]
    CFound -->|yes| CStatus{"Status == 'completed'?"}
    CStatus -->|yes| Resume["OutcomeResume"]
    CStatus -->|no| CTime{"now - StartedAt > CITimeout?"}
    CTime -->|yes| Timeout["OutcomeTimeout"]
    CTime -->|no| Pending["OutcomePending"]

    Resume --> RF{"resume != nil?"}
    RF -->|yes| Call["ResumeFunc(ctx, Action) -> lint-fixer HandleResume"]
    RF -->|no| Skip["leave (not yet wired)"]
    Timeout --> Notify["notifyTimeout -> notifier.Notify<br/>'Lint-fixer needs human review' + PR.URL"]
    NoCheck --> Next["next scan / webhook fast path"]
    Pending --> Next

    Call --> Collect["append Action -> []Action"]
    Notify --> Collect
    Next --> Collect
    Skip --> Collect
    Collect --> Done["return actions, joinErrs(errs)<br/>(§8 recovery for missed CI webhooks)"]
```

- `resume` — check completed → call `ResumeFunc` (lint-fixer decides pass/fail).
- `timeout` — pending past `CITimeout` → notify "needs human review" + PR link.
- `pending` / `nocheck` — leave for the next scan or the webhook fast path.

Consumer-defined `GitHub` interface (faked in tests). `ResumeFunc` is wired by the
lint-fixer in a later phase. Deterministic tooling — no agent imports. Fully tested
with fakes; per-repo errors are collected, not fatal.
