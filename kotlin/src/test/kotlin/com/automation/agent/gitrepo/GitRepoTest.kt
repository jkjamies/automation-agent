package com.automation.agent.gitrepo

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import org.eclipse.jgit.api.Git
import org.eclipse.jgit.lib.PersonIdent
import java.io.File
import java.nio.file.Files
import java.time.Instant
import java.time.ZoneId

/** Creates a local repo with one commit to act as the clone source. */
private fun seedRemote(): String {
    val dir = Files.createTempDirectory("remote").toFile()
    Git.init().setDirectory(dir).call().use { git ->
        File(dir, "README.md").writeText("hi")
        git.add().addFilepattern("README.md").call()
        git.commit()
            .setMessage("init")
            .setAuthor(PersonIdent("seed", "s@x", Instant.ofEpochSecond(1), ZoneId.systemDefault()))
            .call()
    }
    return dir.path
}

/** A fresh, non-existent working-tree path. */
private fun workPath(name: String): String = File(Files.createTempDirectory("gr").toFile(), name).path

class GitRepoTest : BehaviorSpec({
    Given("a seeded remote") {
        When("cloning, branching, committing and pushing") {
            Then("the commit is HEAD and the remote receives the branch") {
                val remote = seedRemote()
                val work = workPath("work")
                val r = Repo.clone(remote, work, "")
                r.checkout("agent/fix", create = true)
                File(r.path("fix.txt")).writeText("patched")

                val sha = r.commitAll("apply fix", Author("agent", "a@x"))
                r.head() shouldBe sha
                r.dir() shouldBe work

                r.push()
                r.push() // a second push with no new commits is up-to-date, not an error

                Git.open(File(remote)).use { rr ->
                    rr.repository.resolve("refs/heads/agent/fix")?.name shouldBe sha
                }
            }
        }
    }

    Given("a pushed remote branch") {
        When("a second clone checks out the existing remote branch") {
            Then("its HEAD matches and a missing branch fails") {
                val remote = seedRemote()
                val r1 = Repo.clone(remote, workPath("w1"), "")
                r1.checkout("feature", create = true)
                File(r1.path("f.txt")).writeText("x")
                val sha = r1.commitAll("feat", Author("a", "a@x"))
                r1.push()

                val r2 = Repo.clone(remote, workPath("w2"), "")
                r2.checkoutRemote("feature")
                r2.head() shouldBe sha
                shouldThrow<Exception> { r2.checkoutRemote("does-not-exist") }
            }
        }
    }

    Given("a freshly cloned repo") {
        When("checking out a missing branch without creating it") {
            Then("it fails") {
                val r = Repo.clone(seedRemote(), workPath("w"), "")
                shouldThrow<Exception> { r.checkout("does-not-exist", create = false) }
            }
        }
    }

    Given("a clean working tree") {
        When("committing with no changes") {
            Then("it raises NoChangesException") {
                val r = Repo.clone(seedRemote(), workPath("w"), "")
                shouldThrow<NoChangesException> { r.commitAll("nothing changed", Author("a", "a@x")) }
            }
        }
    }

    Given("a nonexistent source") {
        When("cloning") {
            Then("it fails") {
                shouldThrow<Exception> {
                    Repo.clone(workPath("does-not-exist"), workPath("nope"), "")
                }
            }
        }
    }
})
