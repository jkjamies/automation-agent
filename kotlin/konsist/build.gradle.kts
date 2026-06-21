// Dedicated architecture-conformance module. Mirrors the Go reference's ARCH/ package.
// Run via `./gradlew arch` (which wires `:konsist:test`); kept out of the service module's
// unit-test run so architecture checks are a separate, explicit command.
plugins {
    kotlin("jvm") version "2.4.0"
}

repositories {
    mavenCentral()
}

dependencies {
    // Konsist scans the whole Gradle project (all modules) — no compile dependency on the
    // service module is needed; it reads the source directly.
    testImplementation("com.lemonappdev:konsist:0.17.3")
    testImplementation("io.kotest:kotest-runner-junit5:6.1.11")
    testImplementation("io.kotest:kotest-assertions-core:6.1.11")
}

kotlin {
    jvmToolchain(17)
}

tasks.test {
    useJUnitPlatform()
}
