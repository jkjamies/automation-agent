---
type: Standard
title: Go style
description: Google's Go Style Guide is the baseline; this concept records only the project-specific deltas — visibility, dependency admission, and the seams the architecture depends on.
tags: [go, style, conventions, dependencies]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Go style

**The baseline is [Google's Go Style Guide](https://google.github.io/styleguide/go/).** Read
it for anything about how Go code should look and read. It is more thorough and better argued
than a restatement here would be, and restating it would only create a second copy to keep in
sync. It assumes familiarity with [Effective Go](https://go.dev/doc/effective_go), which is the
common baseline underneath both.

It is three documents plus an overview, and they carry different weight — *canonical* means
prescriptive and enduring, *normative* means an agreed reference for reviewers that may change
over time:

| Document | Normative | Canonical | What it is |
|---|---|---|---|
| [Style Guide](https://google.github.io/styleguide/go/guide) | Yes | Yes | The foundation; definitive, and the basis for the other two |
| [Style Decisions](https://google.github.io/styleguide/go/decisions) | Yes | No | Decisions on specific style points, with the reasoning behind them |
| [Best Practices](https://google.github.io/styleguide/go/best-practices) | No | No | Patterns that solve common problems and survive maintenance |

The Style Guide's five principles are given **in order of importance**, which is the part worth
internalizing — the order is what resolves a conflict between two of them:

1. **Clarity** — the code's purpose *and rationale* are clear to the reader, judged through the
   reader's eyes rather than the author's.
2. **Simplicity** — it reaches its goal the simplest way, and complexity that is genuinely
   needed is added deliberately and explained.
3. **Concision** — a high signal-to-noise ratio.
4. **Maintainability** — it can be changed correctly by someone who did not write it.
5. **Consistency** — it looks like the code around it. Real, but it loses to the four above.

**Adopting it is not a mandate to churn existing code.** The guide says so itself: adherence
"is not intended to be absolute", the documents "will never be exhaustive", and they explicitly
do not intend to "justify large-scale changes to get rid of style differences" — write new code
to the guide and fix nearby issues as you pass through. A diff whose only content is style
alignment is not the goal.

The guide draws the line precisely, and it is the line to apply here too: matching the
surrounding code is a valid reason to deviate **until** a change "would worsen an existing style
deviation, expose it in more API surfaces, expand the number of files in which the deviation is
present, or introduce an actual bug". At that point local consistency stops being a defense and
the deviation gets cleaned up as part of the change.

`make lint` mechanically enforces the subset a linter can check — `revive` and `staticcheck`
(which on the golangci-lint v2 schema subsumes the former `stylecheck` `ST*` diagnostics), plus
`gofmt`/`goimports` reported as findings. See [CI gates](/tooling/ci-gates.md). A finding fails
the gate, so the mechanical subset needs no prose here either.

## Project deltas

What follows is only what a general Go style guide cannot know about this repository. These are
architecture decisions that happen to be expressed in Go, not matters of style — which is why
they live here rather than being deducible from the guide above.

### Visibility — export the seam, not the machinery

**A type or function is exported only if it appears in the package's contract with the rest of
the service.** Three forms count as contract, and the third is the one that looks like a
violation but isn't:

1. It is named in an exported signature — a parameter, a result, an exported struct field.
2. It is the seam a caller holds. `tasks.Transport` is returned by no exported function in its
   own package, but `cmd/agent` declares variables of it; the interface *is* the contract.
3. It is an **optional-capability interface** a caller type-asserts against.
   `auth.IdentityResolver` appears in no signature anywhere — `cmd/agent` does
   `provider.(auth.IdentityResolver)` to ask whether a provider can resolve its own login.
   Unexporting it would make that question unaskable from outside.

The reason is not aesthetic. This service is built on injected interfaces — `model.LLM`,
`session.Service`, `setup.ParkStore`, each agent's `Deps` — and the [import
boundaries](/standards/architecture.md) that keep provider SDKs out of the agent layer only
hold because callers cannot reach past the seam to the implementation behind it. An exported
implementation type is an invitation to do exactly that.

That leaning on interfaces has a cost the Style Guide names: a maintainer "may need to
understand the specifics of the underlying implementation in order to correctly use the
interface, which must be explained within the interface documentation or at the call-site". So
a seam here carries its contract in its doc comment — `ParkStore` documents that `Sweep` claims
atomically while `SweepOrphans` deliberately does not, because no signature can convey that and
a backend author would otherwise have to infer it.

The pattern, using the [fixflow](/modules/agents/fixflow.md) engine as the worked example:

- `Engine`, `Spec`, `Deps` are exported — they are what `cmd/agent` wires and holds.
- The `driver` behind it, its workflow nodes, and its constructor are unexported. `Engine`
  forwards the lifecycle calls (`Kickoff`, `Resume`, `SweepTimeouts`, `SweepOrphans`), so the
  suspend/resume machinery stays replaceable without touching any caller.
- Those forwarders are one-liners with no policy of their own, and that is fine: their job is
  to be the only door out of the package.

Corollaries worth stating because they are easy to get wrong:

- **An exported type that matches none of the three forms above is a mistake, not a
  convenience.** It is unreachable from outside yet still shows up in `godoc` as public API, so
  it documents a contract that does not exist. `fixflow.Driver` was one until it became
  `fixflow.driver`.
- **Test access is not a reason to export.** Tests live in the same package and see unexported
  identifiers already; an `_test` package that needs an export is a signal the seam is in the
  wrong place.
- **Interfaces are declared by the consumer**, narrow to what that consumer calls, rather than
  exported wholesale by the implementation. `githubapi` exposes a concrete client and each
  caller defines the slice of it that it needs, which is what makes those callers fakeable
  without a shared mock.

### Adding a dependency

This is the local instantiation of the Style Guide's **least mechanism** principle — *"where
there are several ways to express the same idea, prefer the one that uses the most standard
tools"*, escalating from a core language construct, to the standard library, to a new dependency
only when nothing below suffices. The guide's rationale is worth quoting because it is the whole
argument: *"It is easy to add complexity to code as needed, whereas it is much harder to remove
existing complexity after it has been found to be unnecessary."* Maintainability likewise calls
for minimizing dependencies, implicit and explicit.

The libraries in use, and why each was chosen, are catalogued in
[Architecture & Build Plan §3](/standards/architecture-design.md#3-dependencies). What that
principle means concretely here:

- **Prefer the standard library**, then a module already in the graph, then a new module. The
  service deliberately has no `gh` CLI dependency — `go-github` and `go-git` do that work
  in-process — and that shape is worth preserving: a subprocess is an install-time dependency on
  every deployment target, which a module is not.
- **Promoting an existing indirect dependency to direct is cheap**; nothing enters `go.sum`
  that was not already there. Adding a genuinely new module is the case that needs
  justification in the PR body.
- **A provider or storage SDK belongs in `internal/agent/setup`**, behind an interface, and
  nowhere else. The [architecture rules](/standards/architecture.md) name the confined SDKs
  explicitly and the `ARCH/` suite fails if one is imported outside that layer — so a new
  backend's SDK must be added to that list as part of the change, not afterwards.
- **`go.mod` stays tidy without the gate fixing it.** `make ci` runs `tidy-check`
  (`go mod tidy -diff`), which reports rather than rewrites; see
  [CI gates](/tooling/ci-gates.md).

### Configuration and layering

- **Only `internal/config` reads the environment.** Enforced by the `ARCH/` suite across the
  whole module, `cmd/` included.
- **Small packages with one responsibility.** A package that needs an agent import to do its
  job is in the wrong layer — see [Architecture rules](/standards/architecture.md).
- **No global mutable state.** Collaborators arrive through a `Deps` struct so they can be
  faked; nothing reaches for a package-level singleton.
