rootProject.name = "automation-agent-kotlin"

// Architecture-conformance tests live in their own module so they run via a dedicated
// command (`./gradlew arch`) and not with the normal unit tests — the analogue of the Go
// reference's ARCH/ package run by `make arch`.
include(":konsist")
