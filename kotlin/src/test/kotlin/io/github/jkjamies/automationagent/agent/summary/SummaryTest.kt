package io.github.jkjamies.automationagent.agent.summary

import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import io.github.jkjamies.automationagent.agent.setup.drive
import io.github.jkjamies.automationagent.agent.setup.newRunner
import io.github.jkjamies.automationagent.agent.setup.safeName
import io.github.jkjamies.automationagent.githubapi.Commit
import io.github.jkjamies.automationagent.notify.Message
import io.github.jkjamies.automationagent.notify.Notifier
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import io.kotest.matchers.string.shouldNotContain
import io.kotest.matchers.string.shouldStartWith
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.emptyFlow
import java.time.Instant

private class FakeLister(
    private val byRepo: Map<String, List<Commit>> = emptyMap(),
    private val error: Throwable? = null,
) : CommitLister {
    override suspend fun listCommitsSince(owner: String, repo: String, since: Instant): List<Commit> {
        error?.let { throw it }
        return byRepo["$owner/$repo"] ?: emptyList()
    }
}

private class FakeNotifier : Notifier {
    val msgs = mutableListOf<Message>()
    override suspend fun notify(message: Message) {
        msgs += message
    }
}

/** A Model that produces no output — structure/plumbing tests never need real generation. */
private object StubModel : Model {
    override val name: String = "stub"
    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> = emptyFlow()
}

private fun commit(sha: String, message: String, author: String): Commit =
    Commit(sha = sha, message = message, author = author, url = "", at = Instant.EPOCH)

class SummaryTest : BehaviorSpec({
    Given("the formatCommits helper") {
        When("formatting empty and populated commit lists") {
            Then("it summarizes one line per commit and keeps only the first message line") {
                formatCommits("o/r", emptyList()) shouldContain "no commits"
                val got = formatCommits("o/r", listOf(commit("abcdef1234", "fix bug\n\ndetails", "Jane")))
                got shouldContain "abcdef1"
                got shouldContain "fix bug"
                got shouldContain "Jane"
                got shouldNotContain "details"
            }
        }
    }

    Given("commit data in state") {
        When("building the instruction") {
            Then("the prompt leads, non-commit keys are ignored, and sections are sorted") {
                val state = mapOf<String, Any?>("commits:b/b" to "repo B data", "commits:a/a" to "repo A data", "other" to "ignore me")
                val got = buildInstruction("PROMPT", state)
                got shouldStartWith "PROMPT"
                got shouldNotContain "ignore me"
                got.indexOf("repo A data").let { a -> got.indexOf("repo B data").let { b -> (a in 0 until b) shouldBe true } }
            }
        }
        When("there is no commit data") {
            Then("it says so") {
                buildInstruction("P", emptyMap()) shouldContain "no commit data"
            }
        }
    }

    Given("repo strings") {
        When("splitting and sanitizing") {
            Then("valid repos split, invalid ones reject, and names are sanitized") {
                splitRepo("owner/repo") shouldBe ("owner" to "repo")
                splitRepo("bad") shouldBe null
                safeName("a/b:c") shouldBe "a_b_c"
            }
        }
    }

    Given("valid summary deps") {
        When("building the workflow") {
            Then("the composed agent is named summary_workflow") {
                val a = buildSummaryAgent(SummaryDeps(StubModel, FakeLister(), FakeNotifier(), listOf("o/r")))
                a.name shouldBe "summary_workflow"
            }
        }
    }

    Given("summary deps with no repos") {
        When("building the workflow") {
            Then("it fails fast") {
                shouldThrow<IllegalArgumentException> {
                    buildSummaryAgent(SummaryDeps(StubModel, FakeLister(), FakeNotifier(), emptyList()))
                }
            }
        }
    }

    Given("a workflow driven through a real runner with a stub LLM") {
        When("running it") {
            Then("the notifier is invoked exactly once (the fallback digest)") {
                val gh = FakeLister(mapOf("o/r" to listOf(commit("abc1234", "do the thing", "X"))))
                val notifier = FakeNotifier()
                val a = buildSummaryAgent(SummaryDeps(StubModel, gh, notifier, listOf("o/r")))
                drive(newRunner("stub-test", a), "u", "s", "go")
                notifier.msgs shouldHaveSize 1
            }
        }
    }

    Given("a workflow whose fetch fails") {
        When("running it") {
            Then("the error propagates") {
                val a = buildSummaryAgent(SummaryDeps(StubModel, FakeLister(error = RuntimeException("api down")), FakeNotifier(), listOf("o/r")))
                shouldThrow<RuntimeException> { drive(newRunner("stub-test", a), "u", "s", "go") }
            }
        }
    }
})
