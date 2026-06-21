# automation_agent/agent/summary

The summary workflow agent. Build-agent pattern:

## Flow

```mermaid
flowchart TD
    Build["build_summary_agent(Deps{llm, gh, notify, repos})"] --> Seq["SequentialAgent: summary_workflow"]
    Seq --> Par["ParallelAgent: fetch_all"]
    Par --> F1["fetch_<repo1> (code agent)"]
    Par --> Fn["fetch_<repoN> (code agent)"]
    F1 -->|"gh.list_commits_since(now-24h)"| GH[("GitHub")]
    Fn -->|"gh.list_commits_since(now-24h)"| GH
    F1 -->|"state delta commits:<repo1>"| St[("session state")]
    Fn -->|"state delta commits:<repoN>"| St
    Seq --> Smz["summarizer (LlmAgent)"]
    St -->|"instruction provider reads commits:*"| Smz
    Smz -->|"output_key: digest"| St
    Seq --> Ntf["notify (code agent)"]
    St -->|"reads digest"| Ntf
    Ntf --> Chat[("Slack / Teams")]
```

- `agents_setup.py` — `build_summary_agent(Deps)` wires
  `Sequential[ Parallel[fetch x N] -> summarize(LLM) -> notify ]`. Pure wiring.
- `summary.py` — the testable logic: per-repo fetch code-agents, the notify
  code-agent, `format_commits`, and the summarizer's instruction provider.
- `prompts/summarize.md` — the summarizer instruction (markdown, packaged resource).

## Data flow

Each parallel fetcher writes its repo's commit digest to state under
`commits:<owner/repo>`. The summarizer's instruction provider reads all `commits:*`
keys, appends them to the prompt, and the model writes the digest to state under
`digest` (its `output_key`). The notifier reads `digest` and posts it.

`CommitLister` is a consumer-defined protocol over `githubapi` (fakeable). Tests
cover the deterministic helpers and structure; an `OLLAMA_LIVE` test runs the whole
workflow end-to-end against real Gemma. Never assert on LLM output content.
