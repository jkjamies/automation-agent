# internal/githubapi

A thin wrapper over `go-github/v78` exposing only what this service needs:

## Flow

```mermaid
flowchart TD
    Caller[summary / lint-fixer / coverage-fixer / webhook] --> NEW["New(token)"]
    NEW -->|"token != \"\""| AUTH["gh.WithAuthToken(token)"]
    NEW -->|empty token| ANON[unauthenticated client]
    AUTH --> C["Client{gh *github.Client}"]
    ANON --> C

    C --> M1["ListCommitsSince(ctx, owner, repo, since)"]
    C --> M2["CreatePR(ctx, owner, repo, PRInput)"]
    C --> M3["AddLabels(ctx, owner, repo, number, labels...)"]
    C --> M4["FindOpenPRByBranch(ctx, owner, repo, branch)"]
    C --> M6["AgentCheck(ctx, owner, repo, ref, checkName)"]
    C --> M7["GetFileContent(ctx, owner, repo, path, ref)"]
    PCE["ParseCheckRunEvent(body)"] -->|"json.Unmarshal -> CheckEvent"| WH[webhook handler]

    M1 -->|"Repositories.ListCommits (paged)"| GH[(GitHub REST API)]
    M2 -->|PullRequests.Create| GH
    M3 -->|Issues.AddLabelsToIssue| GH
    M4 -->|"PullRequests.List head=owner:branch state=open"| GH
    M6 -->|Checks.ListCheckRunsForRef| GH
    M7 -->|Repositories.GetContents| GH

    M1 -->|"toCommit()"| R1["[]Commit"]
    M2 -->|"toPR()"| R2[PR]
    M4 -->|"toPR (first match)"| R3["PR, found bool"]
    M6 -->|total==0| R5["CheckResult{Found:false}"]
    M6 -->|"CheckRuns[0]"| R6["CheckResult{Status, Conclusion, OutputText}"]
    M7 -->|"fc.GetContent()"| R7[decoded file string]
```

- `ListCommitsSince` — last-24h commit digests (summary workflow).
- `CreatePR` / `AddLabels` — open and label the agent's fix PR.
- `FindOpenPRByBranch` — the open PR for a head branch (used by `apply_fix` to reuse an
  existing agent PR instead of opening a duplicate). Lookup is by branch, not label.
- `AgentCheck` — the agent verify check's status/conclusion for a ref (resume).

Owner/repo are per-call so one client serves many repos. Deterministic tooling — no
agent imports. Tested by pointing a real `*github.Client` at an `httptest` stub
(go-github's `BaseURL` override pattern). Consumers define their own narrow
interfaces over this client for faking.
