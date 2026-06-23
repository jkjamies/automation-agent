package io.github.jkjamies.automationagent.agent.fixflow

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldContain
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.collections.shouldNotContain
import io.kotest.matchers.shouldBe
import java.io.File
import java.nio.file.Files

private fun tempDir(): File = Files.createTempDirectory("fixtools").toFile()

class ToolsTest : BehaviorSpec({
    Given("a checkout with a file") {
        When("reading by repo-relative path") {
            Then("it returns the content and refuses path escapes") {
                val dir = tempDir()
                File(dir, "a.txt").writeText("hello")
                readFile(dir.path, "a.txt") shouldBe "hello"
                shouldThrow<IllegalArgumentException> { readFile(dir.path, "../../etc/passwd") }
            }
        }
    }

    Given("a checkout with files, a subdir and a .git dir") {
        When("listing the root") {
            Then("files and subdirs show (subdirs suffixed), .git is hidden, escapes rejected") {
                val dir = tempDir()
                File(dir, "sub").mkdirs()
                File(dir, ".git").mkdirs()
                File(dir, "f.go").writeText("x")
                val ents = listDirEntries(dir.path, ".")
                ents shouldContain "f.go"
                ents shouldContain "sub/"
                ents shouldNotContain ".git"
                shouldThrow<IllegalArgumentException> { listDirEntries(dir.path, "../..") }
            }
        }
    }

    Given("the safeJoin guard") {
        When("joining paths") {
            Then("traversal and absolute paths are rejected, safe ones accepted") {
                val root = tempDir().path
                for (bad in listOf("../escape", "../../etc/cron.d/x", "/etc/passwd", "a/../../b")) {
                    shouldThrow<IllegalArgumentException> { safeJoin(root, bad) }
                }
                for (ok in listOf("a.go", "sub/dir/b_test.go", ".")) {
                    safeJoin(root, ok) // must not throw
                }
            }
        }
    }

    Given("a symlinked directory inside the checkout pointing outside") {
        When("joining a path through it") {
            Then("it is rejected") {
                val base = tempDir()
                val outside = File(base, "outside").apply { mkdirs() }
                val root = File(base, "root").apply { mkdirs() }
                Files.createSymbolicLink(File(root, "link").toPath(), outside.toPath())
                shouldThrow<IllegalArgumentException> { safeJoin(root.path, "link/x.txt") }
            }
        }
    }

    Given("a dangling symlink pointing outside the checkout") {
        When("joining the link itself") {
            Then("it is rejected") {
                val base = tempDir()
                val root = File(base, "root").apply { mkdirs() }
                // exists but points to a non-existent path outside the root
                Files.createSymbolicLink(File(root, "link").toPath(), File(base, "ghost").toPath())
                shouldThrow<IllegalArgumentException> { safeJoin(root.path, "link") }
            }
        }
    }

    Given("a checkout root") {
        When("building repo tools") {
            Then("it returns read_file and list_dir") {
                repoTools(tempDir().path) shouldHaveSize 2
            }
        }
    }
})
