# CHANGE: <what is changing>

> Kind: change · Status: draft · Date: <YYYY-MM-DD>

## Context
Current behavior and the file(s)/package(s) involved.

## Motivation
Why the current behavior is insufficient.

## Proposed change
Before → after. Be specific about the surface that changes (signatures, config,
prompt content, workflow steps).

## Compatibility
Breaking? Config/env changes? Migration needed (if so, consider `migrate.spec.md`)?

## Test plan
Tests to update/add. Coverage stays ≥80%. No LLM-output assertions.

## Rollback
How to revert.

## Checklist
- [ ] ARCH boundaries respected (`make arch`)
- [ ] `AGENTS.md` updated if behavior/responsibility shifted
- [ ] `make ci` green
- [ ] `docs/architecture.md` updated if the design changed
