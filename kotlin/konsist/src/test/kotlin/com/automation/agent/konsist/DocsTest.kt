package com.automation.agent.konsist

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.booleans.shouldBeTrue
import io.kotest.matchers.collections.shouldBeEmpty
import java.io.File

/**
 * OKF bundle conformance tests: the system's knowledge lives in the repo-root `okf/`
 * bundle (Open Knowledge Format). Structural gate only — every concept opens with YAML
 * frontmatter declaring a non-empty type, every directory carries an `index.md`, every
 * bundle-absolute link resolves, every skill knowledge citation resolves, and the
 * repo-root `AGENTS.md` points at the bundle index.
 */
class DocsTest : BehaviorSpec({
    val repoRoot = generateSequence(File("").absoluteFile) { it.parentFile }
        .first { File(it, "okf").isDirectory }
    val okfRoot = File(repoRoot, "okf")
    val reserved = setOf("index.md", "log.md", "AGENTS.md")

    fun markdownFiles(root: File): List<File> =
        root.walkTopDown().filter { it.isFile && it.extension == "md" }.toList()

    Given("the okf bundle") {
        val concepts = markdownFiles(okfRoot)

        When("scanning the bundle") {
            Then("the scan actually found concept documents") {
                // Guard against a vacuous pass if the bundle walk found nothing.
                concepts.isNotEmpty().shouldBeTrue()
            }
        }

        When("checking concept frontmatter") {
            val typeLine = Regex("(?m)^type:\\s*\\S")
            val bad = concepts
                .filter { it.name !in reserved }
                .mapNotNull { f ->
                    val body = f.readText()
                    if (!body.startsWith("---\n")) return@mapNotNull "${f.path}: missing frontmatter block"
                    val end = body.indexOf("\n---", 4)
                    if (end < 0) return@mapNotNull "${f.path}: frontmatter block not closed"
                    if (typeLine.containsMatchIn(body.substring(4, end))) {
                        null
                    } else {
                        "${f.path}: frontmatter missing required non-empty type field"
                    }
                }
            Then("every concept has a non-empty frontmatter type") {
                bad.shouldBeEmpty()
            }
        }

        When("checking directory indexes") {
            val missing = okfRoot.walkTopDown()
                .filter { it.isDirectory && !File(it, "index.md").isFile }
                .map { it.path }
                .toList()
            Then("every directory has an index.md") {
                missing.shouldBeEmpty()
            }
        }

        When("resolving bundle-absolute links") {
            // Anchor existence inside the target is content, not structure — not validated.
            val link = Regex("]\\((/[^)#]+\\.md)(?:#[^)]*)?\\)")
            val dangling = concepts.flatMap { f ->
                link.findAll(f.readText())
                    .map { it.groupValues[1] }
                    .filter { !File(okfRoot, it).isFile }
                    .map { "${f.path}: $it" }
                    .toList()
            }
            Then("every bundle-absolute link resolves") {
                dangling.shouldBeEmpty()
            }
        }
    }

    Given("the skill files under .agents/skills") {
        val skills = File(repoRoot, ".agents/skills")

        When("resolving knowledge citations in SKILL.md files") {
            val cite = Regex("okf/[A-Za-z0-9._/-]+\\.md")
            val bundlePrefix = okfRoot.canonicalPath + File.separator
            val dangling = if (skills.isDirectory) {
                skills.walkTopDown()
                    .filter { it.isFile && it.name == "SKILL.md" }
                    .flatMap { f ->
                        cite.findAll(f.readText()).map { it.value }.filter { citation ->
                            // A citation must stay inside the bundle — okf/../x.md is not a concept.
                            val target = File(repoRoot, citation).canonicalFile
                            !target.path.startsWith(bundlePrefix) || !target.isFile
                        }.map { "${f.path}: $it" }
                    }
                    .toList()
            } else {
                emptyList()
            }
            Then("every skill knowledge citation points at a concept inside the bundle") {
                dangling.shouldBeEmpty()
            }
        }
    }

    Given("the repo-root AGENTS.md discovery surface") {
        When("reading it") {
            val body = File(repoRoot, "AGENTS.md").readText()
            Then("it points at the bundle index") {
                body.contains("okf/index.md").shouldBeTrue()
            }
        }
    }
})
