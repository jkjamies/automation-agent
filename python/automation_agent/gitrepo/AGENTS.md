# automation_agent/gitrepo

Working-tree git operations via `GitPython`:

## Flow

```mermaid
flowchart TD
    LF[lint-fixer] --> CL["Repo.clone(url, dir, token)"]
    CL --> AF["_auth_url(url, token)"]
    AF -->|"https + token != ''"| BA["x-access-token:token in remote URL"]
    AF -->|"empty token or non-https"| NIL[url unchanged - anonymous]
    CL -->|"GitRepo.clone_from(clone_url, dir)"| REM[(git remote / GitHub)]
    CL --> REPO["Repo{_repo: GitRepo, _dir: str}"]

    REPO --> BR{branch path}
    BR -->|new fix branch| CO["checkout(branch, create=True)"]
    BR -->|retry onto prior fix| COR["checkout_remote(branch)"]
    COR -->|"resolve origin/branch ref"| REM
    COR -->|set ref + checkout| LOCAL[local branch at remote hash]

    CO --> EDIT[write file edits under dir / path rel]
    LOCAL --> EDIT
    EDIT --> CA["commit_all(msg, author)"]
    CA -->|"add(all=True)"| ST{"is_dirty()?"}
    ST -->|clean| NC["raise NoChanges"]
    ST -->|dirty| CMT["index.commit() -> SHA (one commit per attempt)"]
    CMT --> PUSH["push()"]
    PUSH -->|"origin.push (auth from clone URL)"| REM
    PUSH -->|already up to date| OKUP[no-op success]
    CMT --> HEAD["head() -> HEAD SHA"]
```

- `clone(url, dir, token)` — token becomes GitHub `x-access-token` HTTP auth.
- `checkout(branch, create)`, `commit_all(msg, author)` (stages all, returns SHA),
  `push()`, `head()`, `path(rel)`.

The lint-fixer writes file edits under `dir()`, then `commit_all` + `push`. The
invariant **one commit per attempt** lets `githubapi.attempt_count` derive the
iteration count. PR creation lives in `githubapi` (an API op, not a git op).

Deterministic tooling — no agent imports. Tested against a local seed repo, so it
exercises real clone/branch/commit/push without network.
