# Webhooks & CI check names

The canonical registry of the agent's **webhook routes** and the **GitHub `check_run`
names** each workflow uses. When you add, remove, or rename a kickoff route or a verify
check, update the tables here in the same change — they are the human-readable source of
truth that the code must agree with. For the CI-author how-to (workflow YAML, signing,
per-stack examples) see [`ci-integration.md`](ci-integration.md); for the why, see
[`architecture-design.md`](architecture-design.md) §8.

## Two entry doors

Every fixer workflow (lint, coverage, …) is driven by two different events that arrive
through two different doors:

- **Kickoff — a *custom* webhook you control.** Your CI job `POST`s a trusted envelope
  (`{repo, base, report}`) to a **per-workflow** route. You choose the URL and the payload
  shape. HMAC-authenticated with `GITHUB_WEBHOOK_SECRET` when one is set.
- **Resume — GitHub's *native* `check_run` event.** When the agent's verify check completes
  on the PR it opened, GitHub delivers a `check_run` event. A GitHub App has a **single
  webhook URL**, so *every* workflow's resume lands on the one `/webhooks/github` route. The
  agent fans the event to each engine; an engine no-ops unless the incoming check name equals
  its own `CheckName`.

```mermaid
flowchart LR
    CI[Your CI job] -->|POST /webhooks/lint  custom| K[Kickoff: new session]
    GH[GitHub check_run] -->|POST /webhooks/github  native| R[Resume: match by CheckName]
```

So kickoff routing is **per-workflow (by URL)**; resume routing is **shared (one URL, matched
by check name)**.

## Kickoff routes

| Workflow | Kickoff route | ingest `Kind` | Branch | Label |
|---|---|---|---|---|
| Lint | `POST /webhooks/lint` | `KindLint` | `automation-agent/lint-fix` | `automation-agent` |
| Coverage | `POST /webhooks/coverage` | `KindCoverage` | `automation-agent/test-coverage` | `automation-agent` |

## Resume check names (`check_run`)

| Workflow | Verify check name | Branch gate (`head.ref ==`) | Resume route |
|---|---|---|---|
| Lint | `agent-lint-verify` | `automation-agent/lint-fix` | `POST /webhooks/github` |
| Coverage | `agent-coverage-verify` | `automation-agent/test-coverage` | `POST /webhooks/github` |

## The rules (the contract)

- **Check names must be globally unique across all workflows.** The agent routes a
  `check_run` purely by name (`ev.CheckName == spec.CheckName`); two workflows sharing a
  name would cross-fire each other's resumes (a lint check could resume a coverage session).
  Uniqueness is what keeps each task isolated.
- **The agent writes *and* reads the same name.** It creates the verify check on its PR using
  `spec.CheckName`, then later filters resume events on that same constant — so the match is
  guaranteed by construction, not coincidence.
- **Naming patterns.** Verify check: `agent-<workflow>-verify`. Branch: `automation-agent/<slug>`.
  Keep new workflows to these patterns.
- **The label does not distinguish workflows.** Every agent PR shares the single
  `automation-agent` label (used for discovery). The CI *workflow gate* tells workflows apart
  by **branch** (`head.ref`); the agent's *resume routing* tells them apart by **check name**.
  Never rely on the label to route or isolate.

## Where these values live (source of truth)

Each route, `Kind`, branch, and check name is set in the engine `Spec` at construction. Go is
the reference:

- Lint — `go/internal/agent/lintfixer/lint.go` (`CheckName: "agent-lint-verify"`, branch `automation-agent/lint-fix`)
- Coverage — `go/internal/agent/covfixer/coverage.go` (`CheckName: "agent-coverage-verify"`, branch `automation-agent/test-coverage`)

These strings are an **external contract** and must be **identical across all four ports**
(`go/`, `python/`, `kotlin/`, `javascript/`) — see [`language-parity.md`](language-parity.md)
§"External contracts". This doc is the registry; the code is the runtime source; they must
agree. When you change one, change all of them and this table in the same change.

## Adding a new workflow

When you add a fixer (e.g. a `format` or `typecheck` fixer):

1. Choose a **unique** kickoff route, ingest `Kind`, branch (`automation-agent/<slug>`), and
   verify check name (`agent-<name>-verify`). Reuse the shared `automation-agent` label.
2. Register its engine and wire the kickoff route + resume dispatch — in **every** port.
3. Add a row to **both** tables above.
4. Add a CI-author example to [`ci-integration.md`](ci-integration.md).
5. Confirm the new check name collides with no existing one (see the rules above).

## See also

- [`ci-integration.md`](ci-integration.md) — CI-author how-to: workflow YAML, kickoff signing, per-stack examples, the resume verify-check workflows.
- [`architecture-design.md`](architecture-design.md) §8 — why a dedicated, branch-gated agent check exists and how the suspend/resume loop works.
- [`language-parity.md`](language-parity.md) — routes and check names as a cross-port external contract.
