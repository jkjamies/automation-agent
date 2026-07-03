package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.CheckResult
import com.automation.agent.githubapi.CheckRunInput
import com.automation.agent.githubapi.PRFile
import com.automation.agent.githubapi.ReviewCommentRef
import com.automation.agent.githubapi.ReviewInput
import com.automation.agent.githubapi.Tree
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain

private const val PATCH = "@@ -10,2 +10,3 @@\n context\n+added one\n+added two\n"

/** A recording GitHub client for publish assertions. */
private class PubGh(
    private val existing: List<ReviewCommentRef> = emptyList(),
    private val check: CheckResult = CheckResult(found = false),
    private val minimizeThrows: Boolean = false,
) : GitHubClient {
    val reviews = mutableListOf<ReviewInput>()
    val checks = mutableListOf<CheckRunInput>()
    val markers = mutableListOf<String>()
    val minimized = mutableListOf<String>()

    override suspend fun listPRFiles(owner: String, repo: String, num: Int) = emptyList<PRFile>()
    override suspend fun createReview(owner: String, repo: String, num: Int, input: ReviewInput) { reviews.add(input) }
    override suspend fun upsertMarkerComment(owner: String, repo: String, num: Int, marker: String, body: String) { markers.add(body) }
    override suspend fun createCheckRun(owner: String, repo: String, input: CheckRunInput) { checks.add(input) }
    override suspend fun listReviewComments(owner: String, repo: String, num: Int) = existing
    override suspend fun minimizeComment(subjectId: String) {
        if (minimizeThrows) throw RuntimeException("boom")
        minimized.add(subjectId)
    }
    override suspend fun agentCheck(owner: String, repo: String, ref: String, checkName: String) = check
    override suspend fun pullRequestHeadSha(owner: String, repo: String, num: Int) = ""
    override suspend fun tree(owner: String, repo: String, ref: String) = Tree(emptyList(), false)
    override suspend fun getFileContent(owner: String, repo: String, path: String, ref: String) = ""
}

private fun pubEngine(gh: GitHubClient) = Engine(Deps(enabled = true, gh = gh))

private fun meta(files: List<PRFile>, standards: List<String> = emptyList()) =
    PublishMeta(owner = "acme", repo = "api", number = 7, headSha = "sha1", files = files, tiers = "code-reasoning + base", standards = standards)

class PublishTest : BehaviorSpec({
    Given("the external string contracts") {
        Then("the summary marker and check name/conclusions are byte-identical") {
            summaryMarker("acme", "api", 7) shouldBe "<!-- automation-agent:review:acme/api#7 -->"
            CHECK_NAME shouldBe "agent-review"
            checkConclusion(Level.GREEN) shouldBe "success"
            checkConclusion(Level.YELLOW) shouldBe "neutral"
            checkConclusion(Level.RED) shouldBe "neutral"
        }
    }

    Given("findingPrefix and sanitizeText") {
        Then("prefix keys off security then actionable severity") {
            findingPrefix(Finding(dimension = Dimension.SECURITY, severity = Severity.MEDIUM, message = "m")) shouldBe "🔒 Security"
            findingPrefix(Finding(dimension = Dimension.PERFORMANCE, severity = Severity.CRITICAL, message = "m")) shouldBe "⚠️ Potential issue"
            findingPrefix(Finding(dimension = Dimension.PERFORMANCE, severity = Severity.MEDIUM, message = "m")) shouldBe "🛠️ Refactor"
        }
        Then("sanitizeText escapes HTML and breaks @mentions") {
            val out = sanitizeText("a <b> & @user")
            out shouldContain "&lt;b&gt;"
            out shouldContain "&amp;"
            (out.contains("@user")) shouldBe false // a zero-width space is inserted after @
        }
    }

    Given("inlineCommentBody") {
        Then("it renders prefix, suggestion, AI-agent prompt, and the fp marker") {
            val f = Finding(file = "a.kt", line = 11, dimension = Dimension.PERFORMANCE, severity = Severity.MAJOR, message = "slow", suggestion = "fast()", fixPrompt = "make it fast")
            val body = inlineCommentBody(f)
            body shouldContain "**⚠️ Potential issue** · _performance_"
            body shouldContain "```suggestion"
            body shouldContain "🤖 Prompt for AI agents"
            body shouldContain fpMarker(fingerprint(f))
        }
    }

    Given("classify") {
        val idx = DiffIndex(listOf(PRFile(path = "a.kt", patch = PATCH)))
        Then("it routes inline / out-of-diff / nitpick") {
            val inline = Finding(file = "a.kt", line = 11, severity = Severity.MAJOR, message = "in")
            val outside = Finding(file = "a.kt", line = 999, severity = Severity.MAJOR, message = "out")
            val nit = Finding(file = "a.kt", line = 11, severity = Severity.NITPICK, message = "nit")
            val c = classify(listOf(inline, outside, nit), idx)
            c.inline.map { it.message } shouldBe listOf("in")
            c.outOfDiff.map { it.message } shouldBe listOf("out")
            c.nitpicks.map { it.message } shouldBe listOf("nit")
        }
    }

    Given("the summary assembly") {
        Then("scorecardTable states empty vs renders a row") {
            scorecardTable(Scorecard(dims = emptyList(), overall = Level.GREEN, total = 0)) shouldContain "_No findings._"
            val card = scoreFindings(listOf(Finding(dimension = Dimension.SECURITY, severity = Severity.CRITICAL, message = "m")))
            scorecardTable(card) shouldContain "| security |"
        }
        Then("reviewDetails states applied standards or generic") {
            reviewDetails(meta(emptyList())) shouldContain "Standards: generic review"
            reviewDetails(meta(emptyList(), standards = listOf("AGENTS.md"))) shouldContain "Standards applied: AGENTS.md"
        }
        Then("summaryComment includes nitpick and outside-diff sections") {
            val card = scoreFindings(listOf(Finding(dimension = Dimension.PERFORMANCE, severity = Severity.MAJOR, message = "m")))
            val body = summaryComment(
                summaryMarker("acme", "api", 7), card, 2,
                listOf(Finding(file = "a.kt", line = 1, severity = Severity.NITPICK, message = "nit")),
                listOf(Finding(file = "b.kt", line = 2, severity = Severity.MAJOR, message = "out")),
                meta(emptyList()),
            )
            body shouldContain "🧹 Nitpicks (1)"
            body shouldContain "🔭 Outside diff range (1)"
            body shouldContain "Review details"
        }
    }

    Given("publish over a scored PR") {
        When("there are new inline findings and an outdated existing comment") {
            Then("it posts a review, minimizes the gone comment, upserts the summary, and creates the check") {
                val inline = Finding(file = "a.kt", line = 11, dimension = Dimension.PERFORMANCE, severity = Severity.MAJOR, message = "slow")
                val outdated = ReviewCommentRef(nodeId = "n1", body = "old\n" + fpMarker("a.kt:50:gone") + "\n")
                val gh = PubGh(existing = listOf(outdated))
                val e = pubEngine(gh)
                val card = scoreFindings(listOf(inline))
                publish(e, card, listOf(inline), meta(listOf(PRFile(path = "a.kt", patch = PATCH))))
                gh.reviews.size shouldBe 1
                gh.reviews[0].comments.size shouldBe 1
                gh.minimized shouldBe listOf("n1")
                gh.markers.size shouldBe 1
                gh.checks.single().name shouldBe CHECK_NAME
                gh.checks.single().conclusion shouldBe "neutral"
            }
        }
        When("the head SHA is already published") {
            Then("it skips every write") {
                val gh = PubGh(check = CheckResult(found = true))
                publish(pubEngine(gh), scoreFindings(emptyList()), emptyList(), meta(emptyList()))
                gh.checks.size shouldBe 0
                gh.markers.size shouldBe 0
            }
        }
        When("a minimize fails") {
            Then("it logs and still upserts the summary + check") {
                val outdated = ReviewCommentRef(nodeId = "n1", body = "old\n" + fpMarker("a.kt:50:gone") + "\n")
                val gh = PubGh(existing = listOf(outdated), minimizeThrows = true)
                publish(pubEngine(gh), scoreFindings(emptyList()), emptyList(), meta(emptyList()))
                gh.markers.size shouldBe 1
                gh.checks.size shouldBe 1
            }
        }
    }

    Given("publishDeny") {
        Then("it posts the please-split summary and a neutral check") {
            val gh = PubGh()
            publishDeny(pubEngine(gh), meta(emptyList()), "over the file cap", 80, 500000)
            gh.markers.single() shouldContain "too large for automated review"
            gh.checks.single().conclusion shouldBe "neutral"
            gh.checks.single().title shouldContain "too large"
        }
        Then("an already-published deny skips") {
            val gh = PubGh(check = CheckResult(found = true))
            publishDeny(pubEngine(gh), meta(emptyList()), "r", 1, 1)
            gh.markers.size shouldBe 0
        }
    }
})
