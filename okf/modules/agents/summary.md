---
type: Workflow
title: Daily Summary Workflow
description: A scheduled workflow that fetches the last 24 hours of commits across repos in parallel, summarizes them with an LLM, and posts the digest to chat.
resource: go/internal/agent/summary
tags: [summary, daily-digest, notifications]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Daily Summary Workflow

The summary workflow agent, following the build-agent pattern: a builder function does the pure ADK wiring while the testable logic lives in plain functions.

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

## Structure

- `BuildSummaryAgent(Deps)` wires `Sequential[ Parallel[fetch×N] -> summarize(LLM) -> notify ]`. Pure wiring, no logic.
- The testable logic — per-repo fetch code-agents, the notify code-agent, `formatCommits`, and the summarizer's `InstructionProvider` — lives in plain functions separate from the wiring.
- The summarizer instruction is a markdown prompt (`prompts/summarize.md`), embedded in the binary.

## Data flow

Each parallel fetcher writes its repo's commit digest to session state under `commits:<owner/repo>`. The summarizer's instruction provider reads all `commits:*` keys, appends them to the prompt, and the model writes the digest to state under `digest` (its `OutputKey`). The notifier reads `digest` and posts it to chat.

## Reference implementation layout (Go port)

The same structure exists in each port; the Go port is the reference.

- `agents_setup.go` — `BuildSummaryAgent(Deps)`: the sequential/parallel ADK wiring.
- `summary.go` — the testable logic: per-repo fetch code-agents, the notify code-agent, `formatCommits`, the summarizer's `InstructionProvider`.
- `prompts/summarize.md` — the summarizer instruction (markdown, embedded).

## Testing

`CommitLister` is a consumer-defined interface over the GitHub API tooling layer (fakeable). Tests cover the deterministic helpers and structure; a live-gated test runs the whole workflow end-to-end against a real model. Tests never assert on LLM output content.
