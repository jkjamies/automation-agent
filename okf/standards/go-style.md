---
type: Standard
title: Go style
description: Google's Go Style Guide is the baseline; this concept records where each layer of it is enforced and the three project-specific deltas its documents cannot cover.
tags: [go, style, conventions, dependencies, visibility]
sensitivity: internal
bundle: automation-agent
status: stable
generated: { by: human:jkjamies, at: 2026-07-29T00:00:00Z }
---

# Go style

**The baseline is [Google's Go Style Guide](https://google.github.io/styleguide/go/).** Read
it for anything about how Go code should look and read. It is more thorough and better argued
than a restatement here would be, and restating it would only create a second copy to keep in
sync. It assumes familiarity with [Effective Go](https://go.dev/doc/effective_go), the common
baseline underneath both.

It is three documents plus an overview, and they carry different weight — *canonical* means
prescriptive and enduring, *normative* means an agreed reference for reviewers that may change
over time:

| Document | Normative | Canonical | What it is |
|---|---|---|---|
| [Style Guide](https://google.github.io/styleguide/go/guide) | Yes | Yes | The foundation; definitive, and the basis for the other two |
| [Style Decisions](https://google.github.io/styleguide/go/decisions) | Yes | No | Decisions on specific style points, with the reasoning behind them |
| [Best Practices](https://google.github.io/styleguide/go/best-practices) | No | No | Patterns that solve common problems and survive maintenance |

Its five principles are given **in order of importance** — clarity, simplicity, concision,
maintainability, consistency — and the order is the part worth internalizing, because it is
what resolves a conflict between two of them. The consequence people get wrong is the last
one: consistency is real, but it loses to the four above.

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

## Where it is enforced

Three layers, and which layer a rule lands in is a property of the rule, not a matter of taste.

**1. `make lint` — everything a linter can decide.** This is most of Style Decisions.
`go/.golangci.yml` is the map: naming, doc comments, initialisms, import grouping and renaming,
error strings, `ctx` first, no context in a struct, `errors.Is` over `==`, `any` over
`interface{}`, receiver names, indent-error-flow. A finding fails the
[gate](/tooling/ci-gates.md), so none of it needs prose here.

**Every linter and every `revive` rule added to that file carries the guide section it
enforces as an inline citation** — that is what makes the config auditable against its source
instead of an accumulated preference list, and a proposed rule that cannot cite one does not
go in. Two qualifications on that, both visible in the file:

- `revive`'s **default** rule set is kept as a block rather than chosen rule by rule, because
  naming any rule *replaces* the defaults rather than adding to them. Most of those defaults
  cite a section; a few (`empty-block`, `increment-decrement`, `range`, `unreachable-code`) are
  general Go idiom that no section rules on individually. They stay because dropping a default
  is a decision to stop enforcing it, not a neutral edit.
- A linter is only as good as its fidelity to the rule. Where the available linter is **stricter
  than the guide**, it stays out and the rule goes to review, because a gate stricter than its
  source teaches people to ignore it. The file lists these with the reason; the two clearest:
  [Best Practices](https://google.github.io/styleguide/go/best-practices#globals) does not ban
  package state, it gives litmus tests permitting it when logically constant — which is what
  the reviewer's dimension tables are — so `gochecknoglobals` would over-report; and the same
  section warns about init-*order*-dependent state while
  [when-to-panic](https://google.github.io/styleguide/go/best-practices#when-to-panic) takes for
  granted that `init` exists, so `gochecknoinits`' blanket ban is a house rule, not the guide's.
  `lll` is out for the plainest reason of all: the Style Guide says there is no fixed line
  length for Go.

**2. `go/ARCH/` — the structural rules a linter cannot see.** A linter analyses one package at
a time. Every rule in [Architecture rules](/standards/architecture.md), and the visibility rule
below, is a claim about how packages relate: which package may import which, which package may
read the environment, whether anything outside a package can reach a given identifier. Those
need a whole-module view, so they are tests over the parsed tree.

**3. Review — the rest.** Clarity, whether an abstraction earns its keep, whether a comment
explains why. No tool decides these, and pretending otherwise by adding a metric linter
(function length, cyclomatic complexity) would substitute a proxy for the judgment the guide
actually asks for.

## Project deltas

What follows is only what a general Go style guide cannot know about this repository. These
are architecture decisions that happen to be expressed in Go — which is why they live here
rather than being deducible from the guide above. Anything Style Decisions or Best Practices
already rules on is a link, not a restatement.

### Visibility — export the seam, not the machinery

The guide already says most of this: keep an interface unexported if it is only used inside its
package, let [the consumer define
it](https://google.github.io/styleguide/go/best-practices#interface-ownership-and-visibility),
keep it small, and do not export a test double to make something testable. Follow that.

The delta is what "used outside the package" means **in a module with no external users**.
Every package here is under `internal/` or `cmd/`, so this repository contains all the callers
there will ever be, and an exported identifier nothing here reaches is not a contract with a
future caller — it is a contract in `godoc` that no one can enter. So:

**An exported identifier must be part of its package's contract, and there are exactly two ways
to qualify.**

1. **The package names it in its own exported API** — a parameter, a result, an exported struct
   field, an exported interface method.
2. **Another package refers to it.** This covers the two cases that look like violations and
   are not: a seam a caller names rather than receives (`tasks.Transport` is returned by no
   exported function in its own package — `cmd/agent`'s own `buildTransport` is what returns
   it) and an optional-capability interface a caller type-asserts against
   (`auth.IdentityResolver` appears in no signature anywhere; `cmd/agent` does
   `provider.(auth.IdentityResolver)` to ask whether a provider can resolve its own login).

Both of those are producer-exported interfaces, which the guide's default — the consumer
defines the interface — would seem to argue against. They qualify under its own exception:
**the interface is the product**. `tasks.Transport` is one protocol that `CloudTasks` and
`InProcess` both implement, and Best Practices names exactly that case, along with the reason
the producer should own the doc comment when it happens — the "critical behaviors (e.g.,
expected use case, edge cases, concurrency) that need to be centrally and canonically
explicated."

`TestExportedIdentifiersAreReachable` in `go/ARCH/` enforces this. Two consequences of how it
is written are worth knowing before you argue with it:

- **An exported method on an unexported receiver is not exported API.** A type nothing outside
  can name does not become part of the contract by having capitalized methods. That single
  distinction is what gives the check teeth.
- **Your own package's tests are not a reason to export anything** — they sit in the same
  package and see unexported identifiers already. Another package's tests *are* a reason,
  because they are another package, compiling against the seam like any caller.

The reason any of this matters is structural rather than aesthetic. The service is built on
injected interfaces — `model.LLM`, `session.Service`, `setup.ParkStore`, each agent's `Deps` —
and the [import boundaries](/standards/architecture.md) that keep provider SDKs out of the
agent layer only hold because callers cannot reach past the seam to the implementation behind
it. An exported implementation type is an invitation to do exactly that. The worked example is
the [fixflow](/modules/agents/fixflow.md) engine: `Engine`, `Spec` and `Deps` are exported
because `cmd/agent` wires and holds them, while the `driver` behind it, its workflow nodes and
its constructor are not, so the suspend/resume machinery stays replaceable without touching a
caller.

There is one more reason to keep the surface honest, and it is not about readers:
**exporting something hides it from `unused`.** The linter cannot prove an exported identifier
has no callers, so dead code survives indefinitely behind a capital letter. Unexporting is what
lets the gate find it.

### Adding a dependency

This is the local instantiation of the Style Guide's **least mechanism** principle — *"where
there are several ways to express the same idea, prefer the one that uses the most standard
tools"*, escalating from a core language construct, to the standard library, to a new
dependency only when nothing below suffices. The guide's rationale is worth quoting because it
is the whole argument: *"It is easy to add complexity to code as needed, whereas it is much
harder to remove existing complexity after it has been found to be unnecessary."*

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

### Configuration

**Only `internal/config` reads the environment**, enforced by the `ARCH/` suite across the whole
module, `cmd/` included. This is [the flags
rule](https://google.github.io/styleguide/go/decisions#flags) applied to the input this service
actually takes: the guide's point is that a general-purpose package is configured through Go
APIs and never by punching through to the process's own interface, so that importing a library
cannot change how the program is configured as a side effect. A package reading `os.Getenv`
directly does precisely that — and forks configuration away from the typed `Config` and out of
its masked-secret `String` view.
