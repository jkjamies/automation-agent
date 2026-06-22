# automation-agent (Kotlin)

Kotlin port of [`automation-agent`](../README.md), built on
[ADK for Kotlin](https://github.com/google/adk-kotlin). The Go implementation at the repo
root is the canonical reference; this port tracks it **1:1 in functionality**
(see [`../.agents/standards/language-parity.md`](../.agents/standards/language-parity.md)).

> **Status:** complete. Every package is ported with tests, architecture conformance checks
> (`./gradlew arch`), and an 80% coverage floor (`./gradlew koverVerify`).

## Requirements

- JDK 17+
- The Gradle wrapper (`./gradlew`) downloads Gradle 8.12 and all dependencies on first run.

## Quick start

```bash
cp ../.env.example .env    # same env vars as the Go reference
./gradlew build            # compile + test
./gradlew koverVerify      # 80% coverage gate (mirrors `make cover`)
./gradlew run              # run the service (mirrors `make run`)
```

## Design

The architecture is identical to the Go reference and documented once, language-neutrally,
in [`../.agents/standards/architecture-design.md`](../.agents/standards/architecture-design.md). This port mirrors its package
structure, public surface, configuration, and external contracts. See [`AGENTS.md`](AGENTS.md)
for the package map.
