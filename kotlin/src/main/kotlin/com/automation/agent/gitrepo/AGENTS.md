# gitrepo

Wraps [JGit](https://www.eclipse.org/jgit/) for the working-tree operations the fixers need:
clone, branch, stage-all, commit, push. Pure-JVM (no git binary). Deterministic tooling —
**no agent imports**.

## Details

- `GitRepo.kt` — `Repo`: `Repo.clone(url, dir, token)` (suspend, off `Dispatchers.IO`),
  `checkout`, `checkoutRemote`, `commitAll`, `push` (suspend), `head`, `dir`, `path`. `Author`
  is a data class.
- JGit operations used: `Git.cloneRepository`, `checkout().setCreateBranch(…)`,
  `add(".")` + `add(".").setUpdate(true)` to stage all incl. deletions, `status().isClean`,
  `commit()`, `push().add(fullBranch)`.
- A clean tree throws `NoChangesException`; an up-to-date push is not an error (JGit reports
  it without throwing). A token becomes
  `UsernamePasswordCredentialsProvider("x-access-token", token)`.

Tested against a JGit-seeded local repo (clone/branch/commit/push round-trip) — no network.
