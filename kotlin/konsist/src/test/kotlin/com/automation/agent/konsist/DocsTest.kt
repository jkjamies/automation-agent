package com.automation.agent.konsist

import com.lemonappdev.konsist.api.Konsist
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import java.io.File

/**
 * AGENTS.md-presence conformance test. Every directory that contains main Kotlin source must
 * carry an `AGENTS.md`.
 */
class DocsTest : BehaviorSpec({
    Given("every directory that contains main Kotlin source") {
        val mainSourceDirs = Konsist.scopeFromProject().files
            .filter { it.path.contains("/src/main/") }
            .map { File(it.path).parentFile }
            .toSet()
        When("checking each for an AGENTS.md") {
            val missing = mainSourceDirs
                .filter { dir -> !File(dir, "AGENTS.md").exists() }
                .map { it.path }
                .sorted()
            Then("the scan actually found main source directories") {
                // Guard against a vacuous pass if Konsist scanned nothing.
                mainSourceDirs.isNotEmpty() shouldBe true
            }
            Then("each source package directory carries an AGENTS.md") {
                missing shouldBe emptyList()
            }
        }
    }
})
