package com.automation.agent.agent.fixflow

import com.automation.agent.githubapi.Pr
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.kotest.matchers.shouldNotBe
import org.eclipse.jgit.api.Git
import java.io.File
import java.nio.file.Files

class ApplyFixTest : BehaviorSpec({
    Given("a seeded remote") {
        When("applying a fix") {
            Then("it creates and labels the PR and pushes the agent branch") {
                val remote = seedRemote()
                val gh = FakeGitHub()
                val res = applyFix(gh, applyCfg(remote), listOf(FileEdit("internal/foo.kt", "package foo\n")))
                res.pr.number shouldBe 42
                res.headSha shouldNotBe ""
                gh.created?.head shouldBe "agent/fix"
                gh.labeled shouldBe listOf("automation-agent")
                Git.open(File(remote)).use { it.repository.resolve("refs/heads/agent/fix") shouldNotBe null }
            }
        }
    }

    Given("an existing agent PR on the branch") {
        When("applying a retry (reusing the branch)") {
            Then("it reuses the PR rather than creating a new one") {
                val remote = seedRemote()
                applyFix(FakeGitHub(), applyCfg(remote), listOf(FileEdit("a.kt", "package a\n")))
                val retry = applyCfg(remote).copy(newBranch = false)
                val gh = FakeGitHub(existing = listOf(Pr(number = 9, title = "", branch = "agent/fix", headSha = "", url = "", labels = emptyList())))
                val res = applyFix(gh, retry, listOf(FileEdit("b.kt", "package b\n")))
                res.pr.number shouldBe 9
                gh.created shouldBe null
            }
        }
    }

    Given("no edits") {
        When("applying") {
            Then("it fails (no edits / unreachable clone source)") {
                shouldThrow<Exception> { applyFix(FakeGitHub(), applyCfg("x"), emptyList()) }
            }
        }
    }

    Given("an unreachable clone source") {
        When("applying") {
            Then("the clone error propagates") {
                val bad = applyCfg(File(Files.createTempDirectory("nope").toFile(), "missing").path)
                shouldThrow<Exception> { applyFix(FakeGitHub(), bad, listOf(FileEdit("x.kt", "package x\n"))) }
            }
        }
    }

    Given("a failing PR creation") {
        When("applying") {
            Then("the error propagates") {
                val gh = FakeGitHub(createErr = RuntimeException("create failed"))
                shouldThrow<Exception> { applyFix(gh, applyCfg(seedRemote()), listOf(FileEdit("x.kt", "package x\n"))) }
            }
        }
    }
})
