---
type: Decision
title: Gemini-on-Vertex model provider
description: Why production runs Gemma through the native Gemini client on Vertex, with an AI Studio override and Ollama as the local path, behind one builder seam and no proxy layer.
tags: [decision, llm, gemini, vertex, ollama]
sensitivity: internal
bundle: automation-agent
status: accepted
decided: 2026-06-22
timestamp: 2026-07-04T00:00:00Z
---

# Gemini-on-Vertex model provider

## Context

Development is local-first on Ollama + Gemma, but the persistent GCP deployment needs a
hosted model: Ollama in production means operating a GPU VM. Every agent already
receives its model through one builder seam (`LLM_PROVIDER` selects the provider inside
the [setup layer](/modules/agents/setup.md)); the question was what the production
provider should be and whether to unify providers behind a proxy.

## Decision

- **Production default: Gemma served through the native Gemini client on Vertex**
  (`LLM_PROVIDER=gemini`, ADC on Cloud Run). A trivial env override points the same
  client at AI Studio instead of Vertex.
- **Local path: Ollama stays** as the development default and the fallback plan — the
  hand-rolled Ollama adapter forwards tools and generation config, so local Gemma makes
  real tool calls.
- **No proxy or compatibility shim** between the app and the model: two first-class
  providers behind the one `BuildLLM` seam; switching is a config change, never a code
  change. Provider SDKs remain confined to the `agent/setup` layer (ARCH-enforced).
- **Model-size split**: code reasoning and code changes use the larger model
  (`OLLAMA_CODE_MODEL`, 26 B class); summarization and lighter reasoning use the base
  model (`OLLAMA_MODEL`, 12 B class).

## Consequences

- No GPU VM to operate; the production model scales to zero with the service.
- Tool-calling fidelity is native on both providers — no lossy translation layer to
  debug.
- Local development stays free and offline-capable.

## Alternatives considered

- **An OpenAI-compatible proxy fronting both providers** — rejected: an extra hop, an
  extra deployment, and a lossy tool-call translation for zero API-surface benefit,
  since the builder seam already isolates providers.
- **Ollama on a GPU VM in production** — rejected: an always-on VM to babysit for a
  bursty workload; kept only as the documented fallback.
