# REMOVE: <thing being removed>

> Kind: remove · Status: draft · Date: <YYYY-MM-DD>

## Context
What this is and where it's used today.

## Motivation
Why it should go (dead code, replaced, risk, cost).

## Impact analysis
- Callers / dependents:
- Config/env no longer needed:
- Data or external state to clean up:

## Removal plan
Order of operations to remove safely (deprecate → remove callers → delete).

## Test plan
Tests to delete and tests to add proving nothing else broke. Keep coverage ≥80%.

## Rollback
How to restore if removal causes fallout.

## Checklist
- [ ] No dangling imports/refs (`make build`, `make vet`)
- [ ] `AGENTS.md` for removed dirs deleted
- [ ] `make ci` green
- [ ] `docs/architecture.md` updated
