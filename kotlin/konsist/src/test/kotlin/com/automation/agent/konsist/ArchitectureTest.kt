package com.automation.agent.konsist

import com.lemonappdev.konsist.api.Konsist
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe

private const val BASE = "com.automation.agent"

/**
 * Import-boundary conformance tests.
 *
 *  * deterministic tooling must never import agent packages;
 *  * provider SDKs (Ollama / ADK-Gemini / genai) live only in `agent.setup`;
 *  * nothing imports the `app` entrypoint package.
 */
class ArchitectureTest : BehaviorSpec({
    val files = Konsist.scopeFromProject().files

    Given("the project source scope") {
        When("collecting scanned files") {
            // Guard against a vacuous pass: the boundary rules below are only meaningful if
            // Konsist actually scanned the service module's source.
            val packages = files.mapNotNull { it.packagee?.name }.toSet()
            Then("it includes the service module's packages") {
                packages.any { it.startsWith("$BASE.notify") } shouldBe true
                packages.any { it.startsWith("$BASE.config") } shouldBe true
            }
        }
    }

    Given("the deterministic tooling packages") {
        val tooling = listOf("auth", "githubapi", "gitrepo", "webhook", "notify", "tasks", "obs")
        When("inspecting every tooling file's imports") {
            val violations = files
                .filter { f -> tooling.any { pkg -> f.packagee?.name?.endsWith(".$pkg") == true } }
                .flatMap { f -> f.imports.map { imp -> f.path to imp.name } }
                .filter { (_, name) -> name.startsWith("$BASE.agent") }
                .map { (path, name) -> "$path imports agent package $name" }
            Then("tooling must not depend on agents") {
                violations shouldBe emptyList()
            }
        }
    }

    Given("every source file outside agent.setup") {
        // Confined to agent.setup: the Ollama client, the concrete ADK Gemini provider, and the
        // genai backend types. Mirrors the Go reference, which confines only `adk/model/gemini`
        // (the concrete provider) — NOT the `adk/model.Model` abstraction. In ADK-Kotlin the
        // `Model`/`LlmRequest`/`LlmResponse` abstractions share the `com.google.adk.kt.models`
        // package with the concrete `Gemini` class, so confine the class, not the whole package:
        // agents legitimately receive a `Model`, exactly as Go agents import `adk/model.Model`.
        fun isProviderSdk(name: String): Boolean =
            name.contains("ollama") ||
                name == "com.google.adk.kt.models.Gemini" ||
                name.contains("google.genai")
        When("inspecting provider-SDK imports") {
            val violations = files
                .filter { f -> f.packagee?.name?.contains(".agent.setup") != true }
                .flatMap { f -> f.imports.map { imp -> f.path to imp.name } }
                .filter { (_, name) -> isProviderSdk(name) }
                .map { (path, name) -> "$path imports provider SDK $name outside agent.setup" }
            Then("provider SDKs are confined to agent.setup") {
                violations shouldBe emptyList()
            }
        }
    }

    Given("every main source file outside the config package") {
        // Only config may reference an OTEL_* env-var literal; the rest of the service takes tracing
        // settings as a typed struct (config is the single environment reader). Comments are stripped
        // first so a KDoc that names `OTEL_*` in prose is not a violation — only a string literal that
        // begins with OTEL_ (an env-var read like get("OTEL_...")) counts.
        val otelReadRe = Regex("[\"']OTEL_")
        When("scanning for OTEL_ env-var literals") {
            val violations = files
                .filter { it.path.contains("/src/main/") }
                .filter { it.packagee?.name?.endsWith(".config") != true }
                .filter { otelReadRe.containsMatchIn(stripComments(it.text)) }
                .map { "${it.path} references an OTEL_ env-var literal — only config may read OTEL_*" }
                .sorted()
            Then("only config reads the OTEL_* environment") {
                violations shouldBe emptyList()
            }
        }
    }

    Given("every source file outside the app entrypoint package") {
        When("inspecting imports of the app package") {
            val violations = files
                .filter { f -> f.packagee?.name?.endsWith(".app") != true }
                .flatMap { f -> f.imports.map { imp -> f.path to imp.name } }
                .filter { (_, name) -> name == "$BASE.app" || name.startsWith("$BASE.app.") }
                .map { (path, name) -> "$path imports entrypoint package $name" }
            Then("nothing imports the app entrypoint") {
                violations shouldBe emptyList()
            }
        }
    }
})

/** Strips block and line comments so a comment that names OTEL_* in prose is not mistaken for a read. */
private fun stripComments(src: String): String =
    src
        .replace(Regex("/\\*.*?\\*/", RegexOption.DOT_MATCHES_ALL), "")
        .replace(Regex("//[^\n]*"), "")
