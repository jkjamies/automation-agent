# MIGRATE: <what is moving / restructuring>

> Kind: migrate · Status: draft · Date: <YYYY-MM-DD>

## Context
The current shape (layout, dependency, provider, data location) and the target.

## Motivation
Why migrate now (scale, cost, maintainability, deprecation).

## Target state
The end state. E.g. single-instance → multi-instance + shared store; Ollama →
Gemini; package restructure.

## Migration plan
Stepwise, each step independently shippable and reversible:
1.
2.
3.

## Data / state
What state must be carried over and how (recall: GitHub is the source of truth for
lint-fixer; a store only enters on scale-out — see `docs/architecture.md` §8).

## Test plan
Tests proving parity before/after each step. Coverage ≥80%.

## Rollback
Per-step rollback, and the point of no return (if any).

## Checklist
- [ ] Each step independently green under `make ci`
- [ ] ARCH boundaries respected
- [ ] `AGENTS.md` files moved/updated with the code
- [ ] `docs/architecture.md` updated
