// Kotlin port of automation-agent. Mirrors the Go reference at the repo root 1:1 in
// functionality (see ../.agents/standards/language-parity.md). Built on ADK for Kotlin.
//
// This root project is the service module (mirrors Go cmd/ + internal/). Architecture
// tests live in the separate :konsist module, run via `./gradlew arch`.
plugins {
    kotlin("jvm") version "2.4.0"
    kotlin("plugin.serialization") version "2.4.0"
    id("org.jetbrains.kotlinx.kover") version "0.9.8"
    application
    // ADK for Kotlin generates tools from @Tool methods via KSP (KSP2 — now versioned on its
    // own line, e.g. 2.3.9 for Kotlin 2.4.0). Enable when the agent.setup layer is ported
    // (see the deferred dependencies block below):
    // id("com.google.devtools.ksp") version "2.3.9"
}

group = "com.automation"
version = "0.1.0"

val ktorVersion = "3.5.0"

repositories {
    mavenCentral()
}

dependencies {
    // Coroutines (the goroutine equivalent) + JSON (encoding/json equivalent).
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.10.2")
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.8.1")

    // JGit for the working-tree operations the fixers need (the go-git analogue).
    implementation("org.eclipse.jgit:org.eclipse.jgit:7.7.0.202606012155-r")
    // JGit's Apache MINA sshd transport — gives an ssh-agent / ~/.ssh / known_hosts SshSessionFactory
    // for GIT_TRANSPORT=ssh local-dev clone+push. Same JGit version train as the core lib above.
    // (Unlike the shell-out ports, pure-JVM JGit needs explicit SSH wiring.)
    implementation("org.eclipse.jgit:org.eclipse.jgit.ssh.apache:7.7.0.202606012155-r")

    // SQLite JDBC driver for the durable SESSION_BACKEND=sqlite park store + session service.
    // adk-kotlin ships no database session service (unlike adk-go/adk-js), so both are hand-rolled
    // on raw JDBC here. Loaded only when the sqlite backend is selected.
    implementation("org.xerial:sqlite-jdbc:3.53.2.0")

    // Cloud Firestore client for the serverless SESSION_BACKEND=firestore park store + session
    // service (both hand-rolled, like Go's). Used only when the firestore backend is selected.
    implementation("com.google.cloud:google-cloud-firestore:3.43.1")

    // ADK for Kotlin (the native, coroutine-based SDK; mirrors adk-go). The `agent.setup`
    // layer needs core only — it implements the `Model` interface for the Ollama adapter and
    // drives the in-memory `Runner` (incl. resumability). KSP + the webserver stay deferred:
    // KSP is only needed once a @Tool-annotated agent (root/summary) lands, and the webserver
    // backs the cmd/playground web runner. (Gradle resolves the -jvm variant of this KMP lib.)
    implementation("com.google.adk:google-adk-kotlin-core:0.4.0")

    // Ktor — HTTP client (githubapi, the Ollama adapter) + server (webhook).
    implementation("io.ktor:ktor-client-core:$ktorVersion")
    implementation("io.ktor:ktor-client-cio:$ktorVersion")
    implementation("io.ktor:ktor-client-content-negotiation:$ktorVersion")
    implementation("io.ktor:ktor-serialization-kotlinx-json:$ktorVersion")
    implementation("io.ktor:ktor-server-core:$ktorVersion")
    implementation("io.ktor:ktor-server-cio:$ktorVersion")

    // Kotest — the test framework for all ports' Kotlin tests (BehaviorSpec, Given/When/Then).
    testImplementation("io.kotest:kotest-runner-junit5:6.1.11")
    testImplementation("io.kotest:kotest-assertions-core:6.1.11")
    testImplementation("io.ktor:ktor-client-mock:$ktorVersion") // client tests (githubapi)
    testImplementation("io.ktor:ktor-server-test-host:$ktorVersion") // server tests (webhook)

    // --- Deferred: ADK KSP processor + webserver (mirrors adk-go's tool generation + web UI) ---
    // Activated together with the KSP plugin above when an @Tool-annotated agent lands:
    //   implementation("com.google.adk:google-adk-kotlin-webserver:0.4.0")
    //   ksp("com.google.adk:google-adk-kotlin-processor:0.4.0")
}

kotlin {
    jvmToolchain(17)
}

application {
    // The service entrypoint that wires and runs the agent.
    mainClass.set("com.automation.agent.app.MainKt")
}

// `./gradlew playground` — local-only interactive REPL over the configured model (dev only).
tasks.register<JavaExec>("playground") {
    group = "application"
    description = "Run the local playground REPL over the configured model."
    classpath = sourceSets["main"].runtimeClasspath
    mainClass.set("com.automation.agent.playground.PlaygroundKt")
    standardInput = System.`in`
}

tasks.test {
    useJUnitPlatform() // Kotest runs on the JUnit platform.
}

// Enforce an 80% line-coverage floor (see .agents/standards/testing.md).
kover {
    reports {
        // The Firestore backends are emulator-gated (validated under a real Firestore emulator, not
        // in the default run) and excluded from the coverage floor — mirrors the JS port.
        filters {
            excludes {
                classes(
                    "com.automation.agent.agent.setup.FirestoreParkStore",
                    "com.automation.agent.agent.setup.FirestoreSessionService",
                    "com.automation.agent.agent.setup.SessionFirestoreKt",
                )
            }
        }
        verify {
            rule {
                minBound(80)
            }
        }
    }
}

// `./gradlew arch` — dedicated architecture-conformance command.
// Runs the Konsist checks in the :konsist module, independent of the unit-test run.
tasks.register("arch") {
    group = "verification"
    description = "Run Konsist architecture conformance tests (import boundaries + AGENTS.md presence)."
    dependsOn(":konsist:test")
}
