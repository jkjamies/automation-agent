# Language ports

One design, four implementations, organized as two parity pairs. The full contract —
what must match, what may differ, and the change workflow — is the
[language parity standard](/standards/language-parity.md).

# Modern pair (ADK 2.x — carries the design forward)

* [Go port (reference)](go.md) - The reference implementation of automation-agent, built on ADK for Go (google.golang.org/adk/v2 v2.0.0), forming the modern pair with the Python port on the ADK 2.x line.
* [Python port](python.md) - The Python implementation of automation-agent, built on google-adk 2.x, forming the modern pair with the Go reference port on the ADK 2.x line.

# Frozen pair (feature-frozen, 1:1 with each other)

* [Kotlin port](kotlin.md) - The Kotlin implementation of automation-agent, built on adk-kotlin 0.4.0, forming the feature-frozen pair with the TypeScript port at their current 1:1 behavior.
* [TypeScript port](javascript.md) - The TypeScript implementation of automation-agent, built on @google/adk 1.x, forming the feature-frozen pair with the Kotlin port at their current 1:1 behavior.
