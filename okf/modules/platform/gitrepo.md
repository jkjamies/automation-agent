---
type: Platform-Package
title: Git Working-Tree Operations
description: Clone, branch, commit, and push working-tree git operations with scheme-driven auth and per-operation token refresh.
resource: go/internal/gitrepo
tags: [git, working-tree, auth]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Git Working-Tree Operations

Working-tree git operations via an embedded git library (Go reference port:
`go-git/v5` — pure Go, no git binary):

## Flow

```mermaid
flowchart TD
    LF[lint-fixer] --> CL["Clone(ctx, url, dir, Auth{Provider, Repo, SSHKey})"]
    CL --> AF["authFor(ctx, url, Auth) — by URL scheme, token fetched per op"]
    AF -->|"https + Provider.Token(ctx, repo)"| BA["BasicAuth{x-access-token, token}"]
    AF -->|"https + nil provider / empty token"| NIL[nil auth - anonymous]
    AF -->|"git@ / ssh://"| SSH["sshAuth: explicit key, else ssh-agent, else ~/.ssh/id_*"]
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

- `Clone(ctx, url, dir, Auth{Provider, Repo, SSHKey})` — auth is chosen by the URL scheme,
  not the caller: an `https` remote uses `Provider.Token(ctx, Repo)` (GitHub
  `x-access-token` basic auth, or anonymous when the provider is nil / yields ""); a
  `git@…`/`ssh://…` remote uses `SSHKey` if set, else a running ssh-agent, else the first
  default identity file (`~/.ssh/id_ed25519|id_rsa|id_ecdsa`). Host-key checking stays on
  (the library defaults the callback to the user's `known_hosts`). The scheme is selected
  upstream by `GIT_TRANSPORT` (the engine builds the `git@github.com:…` URL).
- The token is fetched **per git operation** — `Clone` *and* `Push` each call the provider
  — so a short-lived GitHub App installation token (~1h) is always current rather than
  captured stale at clone time. `Provider` is the gitrepo-local `TokenProvider` interface,
  satisfied by the static (PAT) and App providers in
  [auth](/modules/platform/auth.md).
- `Checkout(branch, create)`, `CommitAll(msg, author)` (stages all, returns SHA),
  `Push(ctx)` (re-resolves auth), `Head()`, `Path(rel)`.

The [lint-fixer](/modules/agents/lintfixer.md) writes file edits under `Dir()`, then
`CommitAll` + `Push`. The invariant is **one commit per attempt**; the iteration count
is tracked in the [setup](/modules/agents/setup.md) `ParkStore` record, not derived
from git history. PR creation lives in [githubapi](/modules/platform/githubapi.md)
(an API op, not a git op).

Deterministic tooling — no agent imports. Tested against a local seed repo, so it
exercises real clone/branch/commit/push without network.
