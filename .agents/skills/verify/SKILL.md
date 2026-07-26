---
name: verify
description: Verification pipeline with three scopes — code (default, the local gate + okf conformance), full (adds the Firestore-emulator suite), and okf (bundle conformance only). Use after any code or docs change.
---

# Verify

Prove the change is correct. Three scopes, one skill.

**Parameters**: scope (optional, default `code`)

**Usage examples**:
```text
/verify           # code — the local gate + okf conformance
/verify code      # explicit: same as default
/verify full      # adds the emulator-gated Firestore suite
/verify okf       # knowledge-bundle conformance only
```

**The authoritative gate definitions, coverage rules, and failure interpretation live in `okf/tooling/ci-gates.md` and `okf/standards/testing.md`.** This skill is a thin driver; keep the bundle as the single source of truth.

## Scope: `code` (default)

The local gate plus the okf conformance tests (cheap, and docs drift is easy to miss). Both run from `go/`:

```bash
cd go
make ci          # tidy → vet → lint → arch → test → cover (≥80%)
make docs-check  # okf bundle conformance
```

`make ci` already includes the architecture tests, so `docs-check` is only additive when the change is docs-only — run it anyway; it costs a second.

### When to use

- During iterative development — the default inner loop
- For okf/-only changes (`make docs-check` catches broken links and frontmatter)

## Scope: `full`

Everything in `code`, plus the emulator-gated Firestore suite:

```bash
/run-firestore-tests
```

Use before opening a PR, and **always** when the change touches the session service, the park store, or the workflow event fields those stores persist. Those backends are *skipped*, not failed, without an emulator — so a break there is silent in the default gate. This mirrors CI, which runs the two as separate jobs.

## Scope: `okf`

Bundle conformance only — frontmatter `type` on every concept, `index.md` per directory, bundle-absolute links resolve:

```bash
cd go && make docs-check
```

Use after editing anything under `okf/`.

## Key Rules

- **Gates run from `go/`** — never from the repo root.
- **Coverage <80% is a failure**, not a warning. Firestore-emulator-only code is excluded from the default gate's denominator; validate it with `/run-firestore-tests`.
- **Fix, don't suppress** — no skipping tests or lowering thresholds to get green.
- Escalation path: `code → full`. Don't pay for `full` on every save — but never skip it on a session/park-store change.
