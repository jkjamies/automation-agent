# src/githubapi

A thin wrapper over `@octokit/rest` exposing only what this service needs:

## Flow

```mermaid
flowchart TD
    Caller[summary / lint-fixer / coverage-fixer / webhook] --> NEW["new Client(token)"]
    NEW -->|"token !== ''"| AUTH["new Octokit({ auth: token })"]
    NEW -->|empty token| ANON["new Octokit() (unauthenticated)"]
    AUTH --> C["Client{gh: Octokit}"]
    ANON --> C
    TEST["Client.withOctokit(fake)"] -.->|tests inject a fake| C

    C --> M1["listCommitsSince(owner, repo, since)"]
    C --> M2["createPr(owner, repo, PRInput)"]
    C --> M3["addLabels(owner, repo, number, ...labels)"]
    C --> M4["findOpenPrByBranch(owner, repo, branch)"]
    C --> M6["agentCheck(owner, repo, ref, checkName)"]
    C --> M7["getFileContent(owner, repo, path, ref)"]
    PCE["parseCheckRunEvent(body)"] -->|"JSON.parse -> CheckEvent"| WH[webhook handler]

    M1 -->|"paginate(repos.listCommits, since)"| GH[(GitHub REST API)]
    M2 -->|pulls.create| GH
    M3 -->|issues.addLabels| GH
    M4 -->|"pulls.list(head='owner:branch', state='open')"| GH
    M6 -->|"checks.listForRef(filter: latest)"| GH
    M7 -->|repos.getContent| GH

    M1 -->|"toCommit()"| R1["Commit[]"]
    M2 -->|"toPr()"| R2[PR]
    M4 -->|"toPr (first match)"| R3["PR | null"]
    M6 -->|total===0| R5["CheckResult{found: false}"]
    M6 -->|"checkRuns[0]"| R6["CheckResult{status, conclusion, outputText}"]
    M7 -->|"base64 decode"| R7[decoded file string]
```

- `listCommitsSince` — last-24h commit digests (summary workflow).
- `createPr` / `addLabels` — open and label the agent's fix PR.
- `findOpenPrByBranch` — the open PR for a head branch (used by `applyFix` to reuse an
  existing agent PR instead of opening a duplicate). Lookup is by branch, not label.
- `agentCheck` — the agent verify check's status/conclusion for a ref (`filter: latest`,
  re-run-safe). Available for a future resume/timeout re-query; not yet wired in.
- `getFileContent` — decoded file contents at a ref (`""` = default branch).

Each method returns its value or `throw`s an `Error`, and all I/O is `async` (every
method returns a `Promise`). Owner/repo are per-call so one client serves many repos.

Deterministic tooling — no agent imports. Tested by injecting an octokit-shaped
fake via `Client.withOctokit` (no live calls); the pure `parseCheckRunEvent` is
tested directly. Consumers define their own narrow interfaces over this client for
faking.
