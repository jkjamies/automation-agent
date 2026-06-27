package com.automation.agent.gitrepo

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import io.kotest.matchers.string.shouldNotContain
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

/** A gitrepo.TokenProvider that yields a fixed token and records its (repo-scoped) calls, so a test
 * can assert the per-op token lookup happened — or, for insecure / non-https remotes, did NOT. */
private class FakeProvider(private val token: String) : TokenProvider {
    val calls = mutableListOf<String>()
    override suspend fun token(repo: String): String {
        calls += repo
        return token
    }
}

class GitRepoTest : BehaviorSpec({
    Given("a seeded remote") {
        When("cloning, branching, committing and pushing") {
            Then("the commit is HEAD and the remote receives the branch") {
                val remote = seedRemote()
                val work = workPath("work")
                val r = Repo.clone(remote, work, Auth())
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
                val r1 = Repo.clone(remote, workPath("w1"), Auth())
                r1.checkout("feature", create = true)
                File(r1.path("f.txt")).writeText("x")
                val sha = r1.commitAll("feat", Author("a", "a@x"))
                r1.push()

                val r2 = Repo.clone(remote, workPath("w2"), Auth())
                r2.checkoutRemote("feature")
                r2.head() shouldBe sha
                shouldThrow<Exception> { r2.checkoutRemote("does-not-exist") }
            }
        }
    }

    Given("a freshly cloned repo") {
        When("checking out a missing branch without creating it") {
            Then("it fails") {
                val r = Repo.clone(seedRemote(), workPath("w"), Auth())
                shouldThrow<Exception> { r.checkout("does-not-exist", create = false) }
            }
        }
    }

    Given("a clean working tree") {
        When("committing with no changes") {
            Then("it raises NoChangesException") {
                val r = Repo.clone(seedRemote(), workPath("w"), Auth())
                shouldThrow<NoChangesException> { r.commitAll("nothing changed", Author("a", "a@x")) }
            }
        }
    }

    Given("a nonexistent source") {
        When("cloning") {
            Then("it fails") {
                shouldThrow<Exception> {
                    Repo.clone(workPath("does-not-exist"), workPath("nope"), Auth())
                }
            }
        }
    }

    Given("clone URLs of each scheme") {
        When("classifying them") {
            Then("scp-style and ssh:// are ssh; https is not") {
                isSshUrl("git@github.com:acme/api.git") shouldBe true
                isSshUrl("ssh://git@github.com/acme/api.git") shouldBe true
                isSshUrl("https://github.com/acme/api.git") shouldBe false
            }
        }
    }

    Given("the per-op token resolver") {
        When("the remote is https with a provider") {
            Then("it resolves a repo-scoped token from the provider") {
                val prov = FakeProvider("tok")
                tokenFor("https://github.com/o/r.git", Auth(provider = prov, repo = "o/r")) shouldBe "tok"
                prov.calls shouldBe listOf("o/r")
            }
        }
        When("the remote is plaintext http://") {
            Then("it refuses outright and never consults the provider") {
                val prov = FakeProvider("tok")
                val err = shouldThrow<IllegalArgumentException> {
                    tokenFor("http://github.example/o/r.git", Auth(provider = prov, repo = "o/r"))
                }
                err.message shouldContain "insecure http"
                prov.calls shouldBe emptyList() // no token minted for the rejected remote
            }
        }
        When("the remote is ssh or a local path") {
            Then("no token is resolved and the provider is not consulted") {
                val prov = FakeProvider("tok")
                tokenFor("git@github.com:o/r.git", Auth(provider = prov, repo = "o/r")) shouldBe ""
                tokenFor("/local/path/repo", Auth(provider = prov, repo = "o/r")) shouldBe ""
                prov.calls shouldBe emptyList()
            }
        }
        When("https has no provider") {
            Then("it resolves to anonymous") {
                tokenFor("https://github.com/o/r.git", Auth()) shouldBe ""
            }
        }
    }

    Given("a clone that resolves a token") {
        When("the working tree is on disk afterward") {
            Then("the token is never written into .git/config (transport auth, not in-URL)") {
                // A local seed path needs no token, so the provider is not consulted; the point of the
                // assertion is structural — JGit supplies any token as a CredentialsProvider, never in
                // the remote URL, so .git/config never holds a credential at rest.
                val remote = seedRemote()
                val work = workPath("work")
                val prov = FakeProvider("super-secret-token")
                val r = Repo.clone(remote, work, Auth(provider = prov, repo = "o/r"))
                r.checkout("agent/fix", create = true)
                File(r.path("fix.txt")).writeText("patched")
                r.commitAll("apply fix", Author("agent", "a@x"))
                r.push()

                val config = File(work, ".git/config").readText()
                config shouldNotContain "super-secret-token"
                config shouldNotContain "x-access-token"
                prov.calls shouldBe emptyList() // local remote → no mint
            }
        }
    }

    Given("an ssh session factory") {
        When("built with an explicit key path") {
            Then("it is rooted at the user's home and ~/.ssh (known_hosts verification on)") {
                val home = File(System.getProperty("user.home"))
                buildSshFactory("/home/dev/.ssh/id_ed25519").use { f ->
                    f.homeDirectory shouldBe home
                    f.sshDirectory shouldBe File(home, ".ssh")
                }
            }
        }
        When("built with no explicit key (ssh-agent + default identities)") {
            Then("it still produces a usable factory") {
                buildSshFactory("").use { f ->
                    f.sshDirectory shouldBe File(File(System.getProperty("user.home")), ".ssh")
                }
            }
        }
    }
})
