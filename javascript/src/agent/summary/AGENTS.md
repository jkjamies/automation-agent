# src/agent/summary

The summary workflow agent. Build-agent pattern:

```mermaid
flowchart TD
    Build["buildSummaryAgent(Deps{llm, gh, notify, repos})"] --> Seq["SequentialAgent: summary_workflow"]
    Seq --> Par["ParallelAgent: fetch_all"]
    Par --> F1["fetch_<repo1> (code agent)"]
    Par --> Fn["fetch_<repoN> (code agent)"]
    F1 -->|"gh.listCommitsSince(now-24h)"| GH[("GitHub")]
    Fn -->|"gh.listCommitsSince(now-24h)"| GH
    F1 -->|"state delta commits:<repo1>"| St[("session state")]
    Fn -->|"state delta commits:<repoN>"| St
    Seq --> Smz["summarizer (LlmAgent)"]
    St -->|"instruction provider reads commits:*"| Smz
    Smz -->|"outputKey: digest"| St
    Seq --> Ntf["notify (code agent)"]
    St -->|"reads digest"| Ntf
    Ntf --> Chat[("Slack / Teams")]
```

- `agentsSetup.ts` — `buildSummaryAgent(Deps)` wires
  `Sequential[ Parallel[fetch × N] -> summarize(LLM) -> notify ]`. Pure wiring.
- `summary.ts` — the testable logic: per-repo fetch code-agents, the notify code-agent,
  `formatCommits`, and the summarizer's instruction provider.
- `prompts/summarize.md` — the summarizer instruction (markdown, loaded from disk).

## Data flow

Each parallel fetcher writes its repo's commit digest to state under
`commits:<owner/repo>`. The summarizer's instruction provider reads all `commits:*`
keys, appends them to the prompt, and the model writes the digest to state under `digest`
(its `outputKey`). The notifier reads `digest` and posts it.

`CommitLister` is a consumer-defined interface over `githubapi` (fakeable). Tests cover
the deterministic helpers, structure, and end-to-end behavior through a real runner with
a fake model. Never assert on LLM output content.
