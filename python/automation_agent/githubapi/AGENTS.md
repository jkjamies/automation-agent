# automation_agent/githubapi

A thin wrapper over `PyGithub` exposing only what this service needs:

## Flow

```mermaid
flowchart TD
    Caller[summary / lint-fixer / coverage-fixer / webhook] --> NEW["new(token)"]
    NEW -->|"token != ''"| AUTH["Github(auth=Token(token))"]
    NEW -->|empty token| ANON[unauthenticated client]
    AUTH --> C["Client{gh: Github}"]
    ANON --> C

    C --> M1["list_commits_since(owner, repo, since)"]
    C --> M2["create_pr(owner, repo, PRInput)"]
    C --> M3["add_labels(owner, repo, number, labels...)"]
    C --> M4["find_open_pr_by_branch(owner, repo, branch)"]
    C --> M5["attempt_count(owner, repo, number)"]
    C --> M6["agent_check(owner, repo, ref, check_name)"]
    C --> M7["get_file_content(owner, repo, path, ref)"]
    PCE["parse_check_run_event(body)"] -->|"json.loads -> CheckEvent"| WH[webhook handler]

    M1 -->|"repo.get_commits(since=...) (paged)"| GH[(GitHub REST API)]
    M2 -->|repo.create_pull| GH
    M3 -->|issue.add_to_labels| GH
    M4 -->|"repo.get_pulls(state='open', head='owner:branch')"| GH
    M5 -->|"pull.get_commits() (paged)"| GH
    M6 -->|repo.get_commit(ref).get_check_runs| GH
    M7 -->|repo.get_contents| GH

    M1 -->|"to_commit()"| R1["list[Commit]"]
    M2 -->|"to_pr()"| R2[PR]
    M4 -->|"to_pr (first match)"| R3["PR | None for branch"]
    M5 --> R4["int = commit count = attempts"]
    M6 -->|total==0| R5["CheckResult{found: False}"]
    M6 -->|"check_runs[0]"| R6["CheckResult{status, conclusion, output_text}"]
    M7 -->|"decoded_content"| R7[decoded file string]
```

- `list_commits_since` — last-24h commit digests (summary workflow).
- `create_pr` / `add_labels` — open and label the agent's fix PR.
- `find_open_pr_by_branch` — the open PR whose head is the given branch, or `None`
  (used by `apply_fix` to reuse an existing fix PR instead of opening a duplicate). Lookup
  is by branch (`head=owner:branch`), not the agent label — the label is write-only.
- `attempt_count` — commits on a PR = distinct agent-pushed SHAs (one commit per
  attempt; re-run-safe). See `.agents/standards/architecture-design.md` §8.
- `agent_check` — the agent verify check's status/conclusion for a ref (resume).

Owner/repo are per-call so one client serves many repos. Deterministic tooling — no
agent imports. Tested by intercepting the GitHub REST API with `respx` (no live calls).
Consumers define their own narrow protocols over this client for faking.
