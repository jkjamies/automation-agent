# internal/gitrepo

Working-tree git operations via `go-git/v5` (pure Go, no git binary):

## Flow

```mermaid
flowchart TD
    LF[lint-fixer] --> CL["Clone(ctx, url, dir, token)"]
    CL --> AF["authFor(token)"]
    AF -->|"token != \"\""| BA["BasicAuth{x-access-token, token}"]
    AF -->|empty| NIL[nil auth - anonymous]
    CL -->|"PlainCloneContext()"| REM[(git remote / GitHub)]
    CL --> REPO["Repo{repo, wt, dir, auth}"]

    REPO --> BR{branch path}
    BR -->|new fix branch| CO["Checkout(branch, create=true)"]
    BR -->|retry onto prior fix| COR["CheckoutRemote(branch)"]
    COR -->|"resolve origin/branch ref"| REM
    COR -->|SetReference + wt.Checkout| LOCAL[local branch at remote hash]

    CO --> EDIT[write file edits under Dir / Path rel]
    LOCAL --> EDIT
    EDIT --> CA["CommitAll(msg, Author)"]
    CA -->|"AddWithOptions{All:true}"| ST{"Status().IsClean()?"}
    ST -->|clean| NC["return ErrNoChanges"]
    ST -->|dirty| CMT["wt.Commit() -> SHA (one commit per attempt)"]
    CMT --> PUSH["Push(ctx)"]
    PUSH -->|"PushContext(auth)"| REM
    PUSH -->|NoErrAlreadyUpToDate| OKUP[no-op success]
    CMT --> HEAD["Head() -> HEAD SHA"]
```

- `Clone(ctx, url, dir, token)` — token becomes GitHub `x-access-token` HTTP auth.
- `Checkout(branch, create)`, `CommitAll(msg, author)` (stages all, returns SHA),
  `Push(ctx)`, `Head()`, `Path(rel)`.

The lint-fixer writes file edits under `Dir()`, then `CommitAll` + `Push`. The
invariant **one commit per attempt** lets `githubapi.AttemptCount` derive the
iteration count. PR creation lives in `githubapi` (an API op, not a git op).

Deterministic tooling — no agent imports. Tested against a local seed repo, so it
exercises real clone/branch/commit/push without network.
