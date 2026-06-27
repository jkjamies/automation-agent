package com.automation.agent.agent.fixflow

import com.automation.agent.githubapi.Pr
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.shouldBe
import com.automation.agent.agent.setup.ParkRecord
import io.kotest.matchers.string.shouldContain
import io.kotest.matchers.string.shouldNotContain
import java.time.Instant
import java.util.concurrent.atomic.AtomicInteger
import kotlin.time.Duration.Companion.hours

// A parked record seeded directly into the store (the test analogue of the old reg.park), with
// valid run params so any params-decoding path stays happy.
private fun seedParked(prKey: String, sessionId: String, callId: String, attempts: Int): ParkRecord =
    ParkRecord(
        sessionId = sessionId,
        prKey = prKey,
        callId = callId,
        attempts = attempts,
        params = runParamsToJson(RunParams(owner = "acme", repo = "api", fullRepo = "acme/api", base = "master", report = "r")),
        parkedAt = Instant.now(),
    )

// A Spec with deterministic fake triage/analyze (no LLM). The analyze varies content per call so
// every attempt is a real commit; [calls] counts attempts.
private fun fixSpec(calls: AtomicInteger, triageThrows: Boolean = false, triageClean: Boolean = false): Spec =
    Spec(
        name = "test", branch = "agent/fix", checkName = "agent-test-verify",
        commitMessage = "fix", prTitle = "Fix", successTitle = "Fix succeeded", reviewTitle = "Needs human review",
        cleanTitle = "Already clean",
        triage = TriageFunc { _, _ ->
            when {
                triageClean -> throw NoWorkException("triage: nothing here")
                triageThrows -> throw RuntimeException("triage boom")
                else -> listOf(FileWork("a.go", listOf("x")))
            }
        },
        analyze = AnalyzeFunc { listOf(FileEdit("a.go", "package a\n// v${calls.incrementAndGet()}\n")) },
    )

private fun engine(remote: String, gh: FakeGitHub, notifier: FakeNotifier, calls: AtomicInteger = AtomicInteger(0)): Engine =
    Engine(fixSpec(calls), Deps(gh = gh, notifier = notifier, maxIter = 3, ciTimeout = 1.hours, cloneUrl = { _, _ -> remote }))

private fun checkBody(conclusion: String, pr: Int, output: String = ""): ByteArray =
    """{"action":"completed","check_run":{"name":"agent-test-verify","status":"completed","conclusion":"$conclusion","pull_requests":[{"number":$pr,"head":{"ref":"agent/fix"}}],"output":{"text":"$output"}},"repository":{"full_name":"acme/api"}}""".toByteArray()

private val kickoffRaw = """{"repo":"acme/api","base":"master","report":"r"}""".toByteArray()

class EngineTest : BehaviorSpec({
    Given("a kickoff") {
        When("the engine handles it") {
            Then("it applies the fix, opens a labeled PR, and parks awaiting CI") {
                val gh = FakeGitHub()
                val e = engine(seedRemote(), gh, FakeNotifier())
                e.kickoff(kickoffRaw)
                gh.created?.head shouldBe "agent/fix"
                gh.labeled shouldHaveSize 1
                e.driver.parkedCount() shouldBe 1
            }
        }
    }

    Given("a kickoff whose triage finds nothing actionable") {
        When("the engine handles it") {
            Then("it finishes clean: no PR, no parked run, a positive notice (not review), and no error") {
                val gh = FakeGitHub()
                val n = FakeNotifier()
                val e = Engine(
                    fixSpec(AtomicInteger(0), triageClean = true),
                    Deps(gh = gh, notifier = n, maxIter = 3, ciTimeout = 1.hours, cloneUrl = { _, _ -> seedRemote() }),
                )
                e.kickoff(kickoffRaw) // returns normally — the dispatcher sees success, not a failure
                gh.created shouldBe null
                e.driver.parkedCount() shouldBe 0
                n.msgs shouldHaveSize 1
                n.msgs[0].title shouldBe "Already clean"
                n.msgs[0].text shouldNotContain "review"
            }
        }
    }

    Given("a kickoff for a repo not in the allowlist") {
        When("the engine handles it") {
            Then("it is rejected and nothing is parked") {
                val gh = FakeGitHub()
                val e = Engine(
                    fixSpec(AtomicInteger(0)),
                    Deps(gh = gh, notifier = FakeNotifier(), repos = listOf("allowed/repo"), cloneUrl = { _, _ -> "unused" }),
                )
                shouldThrow<IllegalArgumentException> { e.kickoff(kickoffRaw) }
                gh.created shouldBe null
                e.driver.parkedCount() shouldBe 0
            }
        }
    }

    Given("a kickoff for a repo in the allowlist") {
        When("the engine handles it") {
            Then("it is accepted, opens a PR, and parks awaiting CI") {
                val gh = FakeGitHub()
                val remote = seedRemote()
                val e = Engine(
                    fixSpec(AtomicInteger(0)),
                    Deps(gh = gh, notifier = FakeNotifier(), repos = listOf("acme/api"), cloneUrl = { _, _ -> remote }),
                )
                e.kickoff(kickoffRaw)
                gh.created?.head shouldBe "agent/fix"
                e.driver.parkedCount() shouldBe 1
            }
        }
    }

    Given("a parked run") {
        When("CI reports success") {
            Then("it notifies success and frees the run") {
                val n = FakeNotifier()
                val e = engine(seedRemote(), FakeGitHub(), n)
                e.kickoff(kickoffRaw)
                e.resume(checkBody("success", 42))
                n.msgs shouldHaveSize 1
                n.msgs[0].title shouldContain "succeeded"
                e.driver.parkedCount() shouldBe 0
            }
        }

        When("CI fails after exhausting MaxIter") {
            Then("it asks for human review and frees the run") {
                val n = FakeNotifier()
                val e = engine(seedRemote(), FakeGitHub(), n)
                e.driver.store.put(seedParked("acme/api#42", "run-x", "c", attempts = 3))
                e.resume(checkBody("failure", 42, "still broken"))
                n.msgs shouldHaveSize 1
                n.msgs[0].title shouldContain "review"
                e.driver.parkedCount() shouldBe 0
            }
        }

        When("CI fails with attempts remaining") {
            Then("it re-applies on the same PR and re-parks without notifying") {
                val gh = FakeGitHub()
                val n = FakeNotifier()
                val e = engine(seedRemote(), gh, n)
                e.kickoff(kickoffRaw)
                gh.existing = listOf(Pr(number = 42, title = "", branch = "agent/fix", headSha = "", url = "", labels = emptyList()))
                gh.created = null
                e.resume(checkBody("failure", 42, "still failing"))
                gh.created shouldBe null
                n.msgs shouldHaveSize 0
                e.driver.parkedCount() shouldBe 1
            }
        }
    }

    Given("a full kickoff -> fail x3 loop") {
        When("each failure is retried until MaxIter") {
            Then("attempts are counted in memory and review is requested after exactly 3 applies") {
                val gh = FakeGitHub(existing = listOf(Pr(number = 42, title = "", branch = "agent/fix", headSha = "", url = "", labels = emptyList())))
                val n = FakeNotifier()
                val calls = AtomicInteger(0)
                val e = engine(seedRemote(), gh, n, calls)
                e.kickoff(kickoffRaw)
                repeat(2) {
                    e.resume(checkBody("failure", 42, "boom"))
                    n.msgs shouldHaveSize 0
                    e.driver.parkedCount() shouldBe 1
                }
                e.resume(checkBody("failure", 42, "boom"))
                n.msgs shouldHaveSize 1
                n.msgs[0].title shouldContain "review"
                e.driver.parkedCount() shouldBe 0
                calls.get() shouldBe 3
            }
        }
    }

    Given("a parked run whose CI never reports") {
        When("the per-run timeout fires") {
            Then("it frees the run, asks for review, and a late webhook is a benign no-op") {
                val n = FakeNotifier()
                val e = engine(seedRemote(), FakeGitHub(), n)
                e.driver.store.put(seedParked("acme/api#42", "run-x", "c", attempts = 1))
                e.driver.onTimeout("acme/api#42")
                n.msgs shouldHaveSize 1
                n.msgs[0].title shouldContain "review"
                e.driver.parkedCount() shouldBe 0
                e.resume(checkBody("success", 42))
                n.msgs shouldHaveSize 1
            }
        }
    }

    Given("a conclusion for an unknown PR") {
        When("resuming") {
            Then("it is ignored") {
                val n = FakeNotifier()
                val e = engine(seedRemote(), FakeGitHub(), n)
                e.resume(checkBody("success", 99))
                n.msgs shouldHaveSize 0
            }
        }
    }

    Given("a check_run event for another engine's check") {
        When("resuming") {
            Then("it is ignored") {
                val n = FakeNotifier()
                val e = engine(seedRemote(), FakeGitHub(), n)
                e.resume("""{"check_run":{"name":"some-other-check","status":"completed","conclusion":"failure"},"repository":{"full_name":"acme/api"}}""".toByteArray())
                n.msgs shouldHaveSize 0
            }
        }
    }

    Given("a triage that fails") {
        When("handling a kickoff") {
            Then("the error propagates and no run is parked") {
                val e = Engine(fixSpec(AtomicInteger(0), triageThrows = true), Deps(gh = FakeGitHub(), ciTimeout = 1.hours, cloneUrl = { _, _ -> seedRemote() }))
                shouldThrow<Exception> { e.kickoff("""{"repo":"acme/api","report":"r"}""".toByteArray()) }
                e.driver.parkedCount() shouldBe 0
            }
        }
    }

    Given("an apply step that fails") {
        When("handling a kickoff with a notifier") {
            Then("a human is asked to review rather than the failure vanishing silently") {
                val n = FakeNotifier()
                val e = Engine(
                    fixSpec(AtomicInteger(0), triageThrows = true),
                    Deps(gh = FakeGitHub(), notifier = n, ciTimeout = 1.hours, cloneUrl = { _, _ -> seedRemote() }),
                )
                shouldThrow<Exception> { e.kickoff("""{"repo":"acme/api","report":"r"}""".toByteArray()) }
                n.msgs shouldHaveSize 1
                n.msgs[0].title shouldContain "review"
                e.driver.parkedCount() shouldBe 0
            }
        }
    }

    Given("an engine") {
        When("reading its label and check name") {
            Then("they match the spec") {
                val e = engine("x", FakeGitHub(), FakeNotifier())
                e.label() shouldBe "automation-agent"
                e.checkName() shouldBe "agent-test-verify"
            }
        }
    }

    Given("the default clone-URL builder (no test override)") {
        When("the transport is https (the default)") {
            Then("it builds the https GitHub URL") {
                val e = Engine(fixSpec(AtomicInteger(0)), Deps(gh = FakeGitHub()))
                e.cloneUrl("acme", "api") shouldBe "https://github.com/acme/api.git"
            }
        }
        When("the transport is ssh") {
            Then("it builds the scp-style git@ URL") {
                val e = Engine(fixSpec(AtomicInteger(0)), Deps(gh = FakeGitHub(), gitTransport = "ssh"))
                e.cloneUrl("acme", "api") shouldBe "git@github.com:acme/api.git"
            }
        }
        When("a cloneUrl override is injected under ssh") {
            Then("the override still wins over the transport branch") {
                val e = Engine(fixSpec(AtomicInteger(0)), Deps(gh = FakeGitHub(), gitTransport = "ssh", cloneUrl = { _, _ -> "injected" }))
                e.cloneUrl("acme", "api") shouldBe "injected"
            }
        }
    }
})
