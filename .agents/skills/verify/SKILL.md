---
name: verify
description: Verification pipeline with three scopes — diff (default, changed ports only + okf conformance), full (all four ports' gates), and okf (bundle conformance only). Use after any code or docs change.
---

# Verify

Prove the change is correct. Three scopes, one skill.

**Parameters**: scope (optional, default `diff`)

**Usage examples**:
```text
/verify           # diff — gates for the ports touched on this branch + okf conformance
/verify diff      # explicit: same as default
/verify full      # all four ports' gates
/verify okf       # knowledge-bundle conformance only
```

**The authoritative gate definitions, coverage rules, and failure interpretation live in `okf/tooling/ci-gates.md` and `okf/standards/testing.md`.** This skill is a thin driver; keep the bundle as the single source of truth.

## Scope table

| Port | Gate (run from the port dir) | Target runtime* |
|---|---|---|
| Go (`go/`) | `make ci` | ~1–2 min |
| Python (`python/`) | `make ci` | ~30s |
| TypeScript (`javascript/`) | `make ci` | ~1 min |
| Kotlin (`kotlin/`) | `gradle build` | ~2–4 min |

*Approximate, machine-dependent. Every gate enforces lint/typecheck, architecture tests, tests, and coverage ≥80% — see `okf/tooling/ci-gates.md` for what each command chains.

## Scope: `diff` (default)

Run only the gates for ports the branch actually touched, plus the okf conformance tests (cheap, and docs drift is easy to miss).

### Steps

1. Detect changed top-level directories:
   ```bash
   git diff origin/main...HEAD --name-only | cut -d/ -f1 | sort -u
   ```
   (Use `git diff --name-only` too if there are uncommitted changes.)
2. For each of `go`, `python`, `javascript`, `kotlin` that appears, run that port's gate from the table above, from that port's directory.
3. Always run the okf bundle conformance check, regardless of what changed:
   ```bash
   cd go && make docs-check
   ```
4. Stop and fix failures before proceeding; re-run the failed gate to confirm.

### When to use

- During iterative development — the default inner loop
- After changes scoped to one or two ports
- For okf/-only changes (step 3 still catches broken links and frontmatter)

## Scope: `full`

All four ports' gates, in the table order. Use before opening a PR, after cross-port parity changes, or when `diff` passed but the change touches shared contracts (webhook payloads, session/park stores, agent topology — parity rules in `okf/standards/language-parity.md`).

Run each port's gate from its own directory; a failure in one port does not excuse skipping the rest — collect all failures, then fix.

## Scope: `okf`

Bundle conformance only — frontmatter `type` on every concept, `index.md` per directory, bundle-absolute links resolve:

```bash
cd go && make docs-check
```

Use after editing anything under `okf/`.

## Key Rules

- **Gates run from the port's directory** — never from the repo root.
- **Coverage <80% is a failure**, not a warning. Firestore-emulator-only code is excluded from the default gate; validate it with `/run-firestore-tests`.
- **Fix, don't suppress** — no skipping tests or lowering thresholds to get green.
- Escalation path: `diff → full`. Don't pay for `full` on every save.
