# automation_agent/gitrepo

Working-tree git operations via `GitPython`:

## Flow

```mermaid
flowchart TD
    LF[lint-fixer] --> CL["Repo.clone(url, dir, token, ssh_key)"]
    CL --> AF["_auth_url(url, token)"]
    AF -->|"https + token != ''"| BA["x-access-token:token in remote URL"]
    AF -->|"empty token or non-https"| NIL[url unchanged - anonymous / ssh]
    CL --> SE["_ssh_env(ssh_key) when url is git@ / ssh://"]
    SE -->|"ssh_key set"| GSC["GIT_SSH_COMMAND=ssh -i key -o IdentitiesOnly=yes"]
    SE -->|"ssh_key empty"| SYS[system git: ssh-agent / default keys / known_hosts]
    CL -->|"GitRepo.clone_from(clone_url, dir, env)"| REM[(git remote / GitHub)]
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

- `clone(url, dir, token, ssh_key)` — auth is chosen by the URL scheme, not the caller. An
  `https` remote uses `token` as GitHub `x-access-token` HTTP auth (anonymous when empty). A
  `git@…`/`ssh://…` remote (built upstream when `GIT_TRANSPORT=ssh`) is left untouched, so
  the system `git` GitPython shells out to authenticates it via ssh-agent, the default
  identity files, and `known_hosts`. A non-empty `ssh_key` (`GIT_SSH_KEY`) pins ssh to that
  key via `GIT_SSH_COMMAND`; GitPython carries that env onto the repo so `push` reuses it.
- `checkout(branch, create)`, `commit_all(msg, author)` (stages all, returns SHA),
  `push()`, `head()`, `path(rel)`.

The lint-fixer writes file edits under `dir()`, then `commit_all` + `push`. The
invariant **one commit per attempt** lets `githubapi.attempt_count` derive the
iteration count. PR creation lives in `githubapi` (an API op, not a git op).

Deterministic tooling — no agent imports. Tested against a local seed repo, so it
exercises real clone/branch/commit/push without network.
