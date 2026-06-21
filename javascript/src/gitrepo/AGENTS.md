# src/gitrepo

Working-tree git operations via `simple-git`:

## Flow

```mermaid
flowchart TD
    LF[lint-fixer] --> CL["Repo.clone(url, dir, token)"]
    CL --> AF["authUrl(url, token)"]
    AF -->|"https + token != ''"| BA["x-access-token:token in remote URL"]
    AF -->|"empty token or non-https"| NIL[url unchanged - anonymous]
    CL -->|"simpleGit().clone(cloneUrl, dir)"| REM[(git remote / GitHub)]
    CL --> REPO["Repo{git: SimpleGit, dir: string}"]

    REPO --> BR{branch path}
    BR -->|new fix branch| CO["checkout(branch, create=true)"]
    BR -->|retry onto prior fix| COR["checkoutRemote(branch)"]
    COR -->|"revparse origin/branch"| REM
    COR -->|"checkout -b branch hash"| LOCAL[local branch at remote hash]

    CO --> EDIT[write file edits under dir / path rel]
    LOCAL --> EDIT
    EDIT --> CA["commitAll(msg, author)"]
    CA -->|"add(--all)"| ST{"status.isClean()?"}
    ST -->|clean| NC["throw NoChangesError"]
    ST -->|dirty| CMT["commit -> head() SHA (one commit per attempt)"]
    CMT --> PUSH["push()"]
    PUSH -->|"push origin branch:branch"| REM
    PUSH -->|already up to date| OKUP[no-op success]
    CMT --> HEAD["head() -> HEAD SHA"]
```

- `clone(url, dir, token)` — token becomes GitHub `x-access-token` HTTP auth.
- `checkout(branch, create)`, `commitAll(msg, author)` (stages all, returns SHA),
  `push()`, `head()`, `path(rel)`.

The lint-fixer writes file edits under `dir()`, then `commitAll` + `push` (one commit
per attempt). PR creation lives in `githubapi` (an API op, not a git op); attempt counts
live in the in-memory parked-run registry, not in GitHub.

Methods return a value or `throw`; committing a clean tree raises `NoChangesError`.
The committer identity is supplied inline (`-c user.name/user.email` plus `--author`)
so commits succeed without a globally configured git user. `head()` resolves the full
SHA via `revparse` because simple-git's `CommitResult.commit` is abbreviated.

Deterministic tooling — no agent imports. Tested against a local seed repo, so it
exercises real clone/branch/commit/push without network.
