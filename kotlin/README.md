# automation-agent (Kotlin)

Kotlin port of [`automation-agent`](../README.md), built on
[ADK for Kotlin](https://github.com/google/adk-kotlin). The four ports share one design;
this port and the TypeScript port are the feature-frozen pair
(see [`../okf/standards/language-parity.md`](../okf/standards/language-parity.md)).
Every package mirrors the shared architecture, with tests, architecture conformance
checks (`./gradlew arch`), and an 80% coverage floor (`./gradlew koverVerify`).

## Requirements

- JDK 17+
- The Gradle wrapper (`./gradlew`) downloads Gradle 9.6.0 and all dependencies on first run.

## Quick start

```bash
cp ../.env.example .env    # same env vars as the other ports
./gradlew build            # compile + test
./gradlew koverVerify      # 80% coverage gate
./gradlew run              # run the service
```

## Design

The architecture is documented once, language-neutrally, in
[`../okf/standards/architecture-design.md`](../okf/standards/architecture-design.md). This port mirrors its package
structure, public surface, configuration, and external contracts. See
[`../okf/modules/ports/kotlin.md`](../okf/modules/ports/kotlin.md) for the package map.
