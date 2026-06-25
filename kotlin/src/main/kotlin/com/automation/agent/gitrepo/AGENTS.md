# gitrepo

Wraps [JGit](https://www.eclipse.org/jgit/) for the working-tree operations the fixers need:
clone, branch, stage-all, commit, push. Pure-JVM (no git binary). Deterministic tooling —
**no agent imports**.

## Details

- `GitRepo.kt` — `Repo`: `Repo.clone(url, dir, token, sshKey = "")` (suspend, off
  `Dispatchers.IO`), `checkout`, `checkoutRemote`, `commitAll`, `push` (suspend), `head`, `dir`,
  `path`. `Author` is a data class.
- JGit operations used: `Git.cloneRepository`, `checkout().setCreateBranch(…)`,
  `add(".")` + `add(".").setUpdate(true)` to stage all incl. deletions, `status().isClean`,
  `commit()`, `push().add(fullBranch)`.
- A clean tree throws `NoChangesException`; an up-to-date push is not an error (JGit reports
  it without throwing).

### Auth (chosen by clone-URL scheme)

`Repo.clone` routes on the URL, so the engine just builds the right URL (`GIT_TRANSPORT`):

- **https** (`https://…`) — a non-empty token becomes
  `UsernamePasswordCredentialsProvider("x-access-token", token)`; empty = anonymous. This is the
  cloud default.
- **ssh** (`git@…` / `ssh://…`) — local-dev convenience. The token is ignored for transport; a
  `SshdSessionFactory` (Apache MINA sshd, from `org.eclipse.jgit.ssh.apache`) is attached via a
  `TransportConfigCallback` (`SshTransport.setSshSessionFactory`) on **both** clone and push, with
  **no** `CredentialsProvider`. The factory mirrors the `ssh` binary: rooted at `~` / `~/.ssh`, host
  keys verified against `~/.ssh/known_hosts` (never disabled). Key resolution: an explicit
  `GIT_SSH_KEY` wins (used as the sole identity, agent off — like `ssh -i`); otherwise a running
  ssh-agent is preferred, then the default `~/.ssh` identity files. The factory is closed in
  `Repo.close()` to release its key cache / SSH client.

> SSH only covers the git transport. The GitHub REST API (open/label PR, read CI) still needs a
> token, so `GIT_TRANSPORT=ssh` without a token warns at startup (see `app/Main.kt`).

Tested against a JGit-seeded local repo (clone/branch/commit/push round-trip) — no network. The
ssh path is unit-tested for scheme routing and factory construction (home/`~/.ssh`), also hermetic.
