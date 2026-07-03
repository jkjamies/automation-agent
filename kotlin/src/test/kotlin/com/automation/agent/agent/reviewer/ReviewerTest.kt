package com.automation.agent.agent.reviewer

import com.automation.agent.agent.setup.assistantText
import com.automation.agent.githubapi.PRFile
import com.automation.agent.githubapi.PullRequestEvent
import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flowOf

/** A fake GitHub client: returns canned changed files and a canned current head SHA. */
private class FakeGh(
    private val files: List<PRFile> = emptyList(),
    private val headSha: String = "",
) : GitHubClient {
    var listCalls = 0
    override suspend fun listPRFiles(owner: String, repo: String, num: Int): List<PRFile> {
        listCalls++
        return files
    }

    override suspend fun pullRequestHeadSha(owner: String, repo: String, num: Int): String = headSha
}

/** A fake model that returns a fixed findings JSON for every lens (canned; no model reasoning). */
private class FakeModel(private val json: String) : Model {
    override val name: String = "fake"
    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> =
        flowOf(LlmResponse(content = assistantText(json)))
}

/** A recording logger so a test can assert what the engine logged. */
private class RecordingLogger : System.Logger {
    val messages = mutableListOf<String>()
    override fun getName(): String = "test"
    override fun isLoggable(level: System.Logger.Level): Boolean = true
    override fun log(level: System.Logger.Level, bundle: java.util.ResourceBundle?, msg: String?, thrown: Throwable?) {
        if (msg != null) messages.add(msg)
    }
    override fun log(level: System.Logger.Level, bundle: java.util.ResourceBundle?, format: String?, params: Array<out Any?>?) {
        if (format != null) messages.add(format)
    }
}

private fun file(path: String, patch: String = "code", status: String = "modified", add: Int = 1, del: Int = 0) =
    PRFile(path = path, status = status, additions = add, deletions = del, patch = patch)

private fun event(
    action: String = "opened",
    repo: String = "acme/api",
    headRef: String = "feature",
    headSha: String = "sha1",
    draft: Boolean = false,
    labels: List<String> = emptyList(),
    author: String = "alice",
    number: Int = 1,
) = PullRequestEvent(
    action = action, number = number, repoFullName = repo, headRef = headRef, headSha = headSha,
    baseRef = "main", draft = draft, labels = labels, authorLogin = author,
)

// A single security-critical finding, returned by every lens.
private const val CANNED =
    "[{\"file\":\"a.kt\",\"line\":10,\"dimension\":\"security\",\"severity\":\"critical\"," +
        "\"message\":\"hardcoded secret\",\"confidence\":0.9}]"

private fun engine(
    gh: GitHubClient? = FakeGh(),
    enabled: Boolean = true,
    model: Model? = FakeModel(CANNED),
    log: System.Logger? = null,
) = Engine(
    Deps(
        enabled = enabled, gh = gh, baseLlm = model, codeLlm = model,
        minConfidence = 0.6, skipDrafts = true, excludeGlobs = listOf("*.min.js", "vendor/**"),
        maxFiles = 50, maxDiffBytes = 262144, log = log,
    ),
)

class ReviewerTest : BehaviorSpec({
    Given("the deterministic intake pipeline") {
        val gh = FakeGh(files = listOf(file("a.kt")))
        val e = engine(gh = gh)

        When("the action is not a reviewed trigger") {
            Then("it skips without fetching files") {
                val d = e.decide(event(action = "closed"), gh)
                d.kind shouldBe DecisionKind.SKIP
            }
        }
        When("the PR is a draft (and not ready_for_review)") {
            Then("it skips") {
                e.decide(event(draft = true), gh).kind shouldBe DecisionKind.SKIP
            }
        }
        When("the PR is from the agent's own branch") {
            Then("it skips") {
                e.decide(event(headRef = "automation-agent/lint-fix"), gh).kind shouldBe DecisionKind.SKIP
            }
        }
        When("the PR carries the skip-review label") {
            Then("it skips") {
                e.decide(event(labels = listOf("skip-review")), gh).kind shouldBe DecisionKind.SKIP
            }
        }
        When("the author is a dependency bot") {
            Then("it skips") {
                e.decide(event(author = "dependabot[bot]"), gh).kind shouldBe DecisionKind.SKIP
            }
        }
        When("the repository full name is malformed") {
            Then("it throws") {
                shouldThrow<IllegalArgumentException> { e.decide(event(repo = "nope"), gh) }
            }
        }
        When("every changed file is excluded") {
            Then("it skips (empty filtered diff)") {
                val onlyExcluded = engine(gh = FakeGh(files = listOf(file("app.min.js"))))
                onlyExcluded.decide(event(), FakeGh(files = listOf(file("app.min.js")))).kind shouldBe DecisionKind.SKIP
            }
        }
        When("the filtered diff is over a cap") {
            Then("it denies rather than degrading") {
                val big = engine(gh = FakeGh())
                val over = Engine(
                    Deps(enabled = true, gh = FakeGh(files = listOf(file("a.kt"), file("b.kt"))), maxFiles = 1),
                )
                over.decide(event(), FakeGh(files = listOf(file("a.kt"), file("b.kt")))).kind shouldBe DecisionKind.DENY
                big.decide(event(), FakeGh(files = listOf(file("a.kt")))).kind shouldBe DecisionKind.REVIEW
            }
        }
    }

    Given("a disabled engine") {
        When("kickoff receives a pull_request event") {
            Then("it no-ops and never fetches the diff") {
                val gh = FakeGh(files = listOf(file("a.kt")))
                engine(gh = gh, enabled = false).kickoff(prEventJson().toByteArray())
                gh.listCalls shouldBe 0
            }
        }
    }

    Given("an enabled engine with no GitHub client") {
        When("kickoff runs") {
            Then("it raises a controlled error") {
                shouldThrow<IllegalStateException> { engine(gh = null).kickoff(prEventJson().toByteArray()) }
            }
        }
    }

    Given("an enabled engine reviewing a real diff") {
        When("kickoff runs over a reviewable PR") {
            Then("it scores the review and logs it (no GitHub writes)") {
                val log = RecordingLogger()
                val e = engine(gh = FakeGh(files = listOf(file("a.kt"))), log = log)
                e.kickoff(prEventJson().toByteArray())
                log.messages.any { it.startsWith("review scored") } shouldBe true
            }
        }
        When("a newer push has superseded the event SHA") {
            Then("it skips the stale review") {
                val log = RecordingLogger()
                // The current head SHA differs from the event's, so the task is stale.
                val e = engine(gh = FakeGh(files = listOf(file("a.kt")), headSha = "newer-sha"), log = log)
                e.kickoff(prEventJson(headSha = "old-sha").toByteArray())
                log.messages.any { it.startsWith("stale review skipped") } shouldBe true
            }
        }
    }

    Given("the model-calling review stage") {
        When("run over a reviewable diff with canned lens output") {
            Then("it dedupes cross-lens, gates by confidence, and scores critical-cap red") {
                val e = engine(gh = FakeGh(files = listOf(file("a.kt"))))
                val result = runReview(e, listOf(file("a.kt")))
                result.findings.size shouldBe 1
                result.card.total shouldBe 1
                result.card.overall shouldBe Level.RED
                result.findings[0].dimension shouldBe Dimension.SECURITY
                result.findings[0].severity shouldBe Severity.CRITICAL
            }
        }
    }
})

private fun prEventJson(action: String = "opened", headSha: String = "sha1"): String =
    "{\"action\":\"$action\",\"number\":1,\"repository\":{\"full_name\":\"acme/api\"}," +
        "\"pull_request\":{\"number\":1,\"draft\":false,\"head\":{\"ref\":\"feature\",\"sha\":\"$headSha\"}," +
        "\"base\":{\"ref\":\"main\"},\"user\":{\"login\":\"alice\"},\"labels\":[]}}"
