---
name: migrate
description: Execute a library, API, or framework swap on a port — mechanical migration that preserves observable behavior, verified against the shipped API. Use when bumping a major SDK version (e.g. the ADK), swapping a library, or moving to a new provider API.
---

# Migrate

Swap the underlying library/API while keeping the system's observable behavior fixed.
The existing tests are the contract: where behavior is unchanged, they pass unchanged.

**Parameters**: migration description (from → to), or a spec via `@file`.

**Usage examples**:
```text
/migrate @specs/20260630-adk2-go-python.md
/migrate go: google.golang.org/adk v1.4 -> /v2 v2.0.0
/migrate python: PyGithub -> githubkit
```

## Pre-flight: Grill the Spec

When a spec is given via `@file`, scan it for unresolved markers (`<!-- TODO -->`, empty
`- [ ]` AC items, empty required header fields, `...` table cells, `presumably`/`appears
to` presumptions). If any are present, **stop and run `/grill-me @{spec}` first**, then
resume. The spec (or grill) must settle: which port(s) the migration covers, whether any
observable behavior is *intended* to change (default: none), and the exact target version.

## Reference knowledge

- Each port targets its own native SDK; parity is functional, not version-matched: okf/standards/language-parity.md
- Provider/SDK confinement (`agent/setup` only) that the migration must not leak past: okf/standards/architecture.md
- The wiring/logic split — migrations mostly land in `agents_setup.*` and `setup/`: okf/standards/agent-build-pattern.md
- External contracts that must survive byte-identical: okf/standards/webhooks.md
- Test gates per port: okf/standards/testing.md
- Docs-move-with-code rule: okf/standards/documentation.md

## Steps

### 1. Inventory every callsite

Grep the target port for the old import path / package / API symbols — source, tests,
manifests (`go/go.mod`, `python/pyproject.toml` + `uv.lock`), and the okf bundle (version
strings and API names appear in concepts, e.g. the parity table). Produce a punch list of
`file:line — what changes` before editing anything.

### 2. Verify against the shipped API — not release notes

Read the actual installed sources of the target version (the module cache /
site-packages, or vendor the new version and read it). Confirm every signature,
constructor, and type on the punch list exists as you expect. Release notes and training
data lie about details; the shipped source does not. Record any surprise (renamed helper,
changed semantics) in the punch list before migrating.

### 3. Pin the new version

Update the manifest (`go.mod` → new module path/version; `pyproject.toml` → new pin) and
sync (`go mod tidy` / `uv sync`). If the import path changes (e.g. a `/v2` module), do the
path rewrite as its own mechanical pass.

### 4. Migrate mechanically, boundary-aware

Work through the punch list in dependency order (setup/ and platform packages first,
agent dirs after). Adapters and provider-touching code stay confined to `agent/setup`
(okf/standards/architecture.md) — a migration is not a license to spread SDK imports.
Keep each intermediate state compiling; don't leave the port broken between passes.

### 5. Existing tests are the contract

Run the full suite. Where behavior is unchanged, tests pass **unchanged** — a test edit is
a red flag to justify, not a chore to bulldoze. Legitimate edits are limited to: fakes
that implement the SDK's interface (the interface moved), and tests that assert on the
old SDK's own types. Any behavioral delta must be intended by the spec and named in the PR.

### 6. Pair rules

Per okf/standards/language-parity.md: if the SDK is **port-specific** (Go's ADK module,
a Go-only library), the migration may legitimately be single-port — **record the pair gap
in the PR description** so the modern pair's drift is declared, not silent. If the
migration is conceptual (both ports move to the same new API family), land Go first and
mirror Python in the same change set. The frozen pair (`kotlin/`, `javascript/`) is never
migrated by feature-work momentum.

### 7. Sweep stale references — knowledge update MANDATORY

Grep the repo and the bundle for the old version/module path. Run `/update-okf`:

- the parity table in okf/standards/language-parity.md if an ADK/module version changed;
- the port concept (okf/modules/ports/) and any concept naming the old library
  (e.g. okf/modules/agents/setup.md, okf/standards/architecture.md's SDK lists);
- per-port `README.md` version mentions.

Docs state the new reality factually — no "upgraded from" narration
(okf/standards/documentation.md).

## Verification

```bash
cd go && make ci            # tidy + vet + arch + test + 80% cover
cd go && make docs-check    # okf bundle conformance
cd python && make ci        # when the migration covers Python
```

Then grep for the old import path / version string one final time — zero hits outside
`specs/`.

## Key Rules

- **Preserve observable behavior** — same routes, payloads, check names, env semantics,
  and test outcomes; the swap is invisible from outside.
- **Verify against shipped source, not memory or release notes** — every API you call,
  you have read.
- **Never break the port mid-migration** — each pass compiles and tests green.
- **Confinement survives the swap** — new SDK imports obey the same setup-only boundary
  as the old ones; `make arch` proves it.
- **Declare pair gaps** — a Go-only migration is fine only when the PR says so and why.
- **Stale version strings are bugs** — the bundle and READMEs move in the same change.
