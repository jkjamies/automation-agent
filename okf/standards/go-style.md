---
type: Standard
title: Go style
description: Idiomatic Go conventions for the service — formatting, errors, naming, dependency injection, configuration, and context.
tags: [go, style, conventions]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-04T00:00:00Z
---

# Go style

Idiomatic Go, matching the surrounding code.

- **Formatting:** `gofmt`/`goimports` clean (`make fmt`); `goimports` local prefix
  is `automation-agent`.
- **Errors:** return errors, don't panic in library code. Wrap with context:
  `fmt.Errorf("doing x: %w", err)`. Handle or explicitly ignore — `errcheck` is on.
- **Naming:** short, lower-case package names; no stutter (`config.Config`, not
  `config.ConfigStruct`). Exported identifiers have doc comments.
- **Dependency injection:** pass collaborators as interfaces/structs (a `Deps`
  struct for agents) so they can be faked in tests. No global mutable state.
- **Configuration:** only `internal/config` reads the environment.
- **Context:** accept `context.Context` as the first parameter for anything doing
  I/O or that can be cancelled.
- **Small packages with one responsibility.** If a package needs an agent import to
  do its job, it's in the wrong layer (see [Architecture rules](/standards/architecture.md)).
