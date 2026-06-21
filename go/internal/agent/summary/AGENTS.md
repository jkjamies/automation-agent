# internal/agent/summary

The summary workflow agent. Build-agent pattern:

## Flow

```mermaid
flowchart TD
    Build["BuildSummaryAgent(Deps{LLM, GH, Notify, Repos})"] --> Seq["SequentialAgent: summary_workflow"]
    Seq --> Par["ParallelAgent: fetch_all"]
    Par --> F1["fetch_<repo1> (code agent)"]
    Par --> Fn["fetch_<repoN> (code agent)"]
    F1 -->|"GH.ListCommitsSince(now-24h)"| GH[("GitHub")]
    Fn -->|"GH.ListCommitsSince(now-24h)"| GH
    F1 -->|"StateDelta commits:<repo1>"| St[("session state")]
    Fn -->|"StateDelta commits:<repoN>"| St
    Seq --> Smz["summarizer (llmagent)"]
    St -->|"InstructionProvider reads commits:*"| Smz
    Smz -->|"OutputKey: digest"| St
    Seq --> Ntf["notify (code agent)"]
    St -->|"reads digest"| Ntf
    Ntf --> Chat[("Slack / Teams")]
```

- `agents_setup.go` — `BuildSummaryAgent(Deps)` wires
  `Sequential[ Parallel[fetch×N] -> summarize(LLM) -> notify ]`. Pure wiring.
- `summary.go` — the testable logic: per-repo fetch code-agents, the notify
  code-agent, `formatCommits`, and the summarizer's `InstructionProvider`.
- `prompts/summarize.md` — the summarizer instruction (markdown, embedded).

## Data flow

Each parallel fetcher writes its repo's commit digest to state under
`commits:<owner/repo>`. The summarizer's instruction provider reads all `commits:*`
keys, appends them to the prompt, and the model writes the digest to state under
`digest` (its `OutputKey`). The notifier reads `digest` and posts it.

`CommitLister` is a consumer-defined interface over `githubapi` (fakeable). Tests
cover the deterministic helpers and structure; an `OLLAMA_LIVE` test runs the whole
workflow end-to-end against real Gemma. Never assert on LLM output content.
