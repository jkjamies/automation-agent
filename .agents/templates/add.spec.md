# ADD: <feature name>

> Kind: add · Status: draft · Date: <YYYY-MM-DD>

## Context
What exists today, and why this addition is needed.

## Motivation
The problem this solves / the value it adds.

## Scope
- In scope:
- Out of scope:

## Design
How it works. New packages/agents/tools, the ingest→root→workflow path it touches,
and any new config/env. Note which `standards/` apply.

## Test plan
Unit tests and the coverage impact (must keep ≥80%). What fakes/stubs are needed.
No assertions on LLM output.

## Rollout / rollback
How it ships and how to back it out if needed.

## Checklist
- [ ] `AGENTS.md` added/updated for new dirs
- [ ] ARCH boundaries respected (`make arch`)
- [ ] `make ci` green
- [ ] Prompts (if any) are markdown under `prompts/`
- [ ] `docs/architecture.md` updated if the design changed
