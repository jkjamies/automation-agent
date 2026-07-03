package com.automation.agent.agent.reviewer

import com.automation.agent.agent.setup.assistantText
import com.automation.agent.githubapi.CheckResult
import com.automation.agent.githubapi.CheckRunInput
import com.automation.agent.githubapi.PRFile
import com.automation.agent.githubapi.ReviewCommentRef
import com.automation.agent.githubapi.ReviewInput
import com.automation.agent.githubapi.Tree
import com.automation.agent.githubapi.TreeEntry
import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import com.google.adk.kt.types.Content
import com.google.adk.kt.types.FunctionCall
import com.google.adk.kt.types.Part
import com.google.adk.kt.types.Role
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flowOf

private class StdModel(private val json: String) : Model {
    override val name: String = "fake"
    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> =
        flowOf(LlmResponse(content = assistantText(json)))
}

/** A fake GitHub client serving a canned tree + file contents for standards discovery. */
private class StdGh(
    private val tree: Tree,
    private val contents: Map<String, String> = emptyMap(),
) : GitHubClient {
    override suspend fun listPRFiles(owner: String, repo: String, num: Int) = emptyList<PRFile>()
    override suspend fun createReview(owner: String, repo: String, num: Int, input: ReviewInput) {}
    override suspend fun upsertMarkerComment(owner: String, repo: String, num: Int, marker: String, body: String) {}
    override suspend fun createCheckRun(owner: String, repo: String, input: CheckRunInput) {}
    override suspend fun listReviewComments(owner: String, repo: String, num: Int) = emptyList<ReviewCommentRef>()
    override suspend fun minimizeComment(subjectId: String) {}
    override suspend fun agentCheck(owner: String, repo: String, ref: String, checkName: String) = CheckResult(found = false)
    override suspend fun pullRequestHeadSha(owner: String, repo: String, num: Int) = ""
    override suspend fun tree(owner: String, repo: String, ref: String) = tree
    override suspend fun getFileContent(owner: String, repo: String, path: String, ref: String) = contents[path] ?: ""
}

private val RULE_JSON =
    "[{\"id\":\"R1\",\"dimension\":\"pattern_violation\",\"summary\":\"wrap errors\",\"source\":\"AGENTS.md\"}]"

/** A model that calls get_rule on its first turn, then returns [json] once the tool result is fed back. */
private class ToolThenTextModel(private val json: String) : Model {
    override val name: String = "fake"
    private var calls = 0
    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> {
        calls++
        return if (calls == 1) {
            flowOf(LlmResponse(content = Content(role = Role.MODEL, parts = listOf(Part(functionCall = FunctionCall(name = "get_rule", args = mapOf("id" to "R1"), id = "get_rule"))))))
        } else {
            flowOf(LlmResponse(content = assistantText(json)))
        }
    }
}

private fun stdEngine(
    gh: GitHubClient? = null,
    model: Model? = StdModel(RULE_JSON),
    enabled: Boolean = true,
    uncitedDrop: Boolean = false,
) = Engine(
    Deps(
        enabled = true, gh = gh, baseLlm = model, codeLlm = model,
        standardsEnabled = enabled, standardsGlobs = listOf("AGENTS.md", ".agents/standards/**"),
        standardsMaxBytes = 1_000_000, uncitedDrop = uncitedDrop,
    ),
)

private fun blob(path: String, sha: String = "s-$path") = TreeEntry(path = path, sha = sha, type = "blob")

class StandardsTest : BehaviorSpec({
    Given("glob matching over a tree") {
        Then("matchStandards keeps only matching blobs, sorted") {
            val entries = listOf(blob("src/Main.kt"), blob("AGENTS.md"), blob(".agents/standards/style.md"), TreeEntry("dir", "x", "tree"))
            matchStandards(entries, listOf("AGENTS.md", ".agents/standards/**")).map { it.path } shouldBe
                listOf(".agents/standards/style.md", "AGENTS.md")
        }
        Then("matchesGlob honors basename vs path globs") {
            val pats = compileStandardsGlobs(listOf("AGENTS.md", "vendor/**", ""))
            matchesGlob(pats, "kotlin/AGENTS.md") shouldBe true
            matchesGlob(pats, "vendor/x/y") shouldBe true
            matchesGlob(pats, "src/Main.kt") shouldBe false
        }
    }

    Given("per-module scoping") {
        Then("moduleScoped is true only for nested instruction files") {
            moduleScoped("AGENTS.md") shouldBe false
            moduleScoped("kotlin/AGENTS.md") shouldBe true
            moduleScoped("kotlin/src/config.yaml") shouldBe false
        }
        Then("touchedDirs walks every ancestor to the root") {
            touchedDirs(listOf(PRFile(path = "kotlin/src/Main.kt"))) shouldBe setOf("kotlin/src", "kotlin", ".")
        }
        Then("scopeToTouched drops instruction files for untouched modules, keeps root ones") {
            val matched = listOf(blob("AGENTS.md"), blob("kotlin/AGENTS.md"), blob("python/AGENTS.md"))
            val changed = listOf(PRFile(path = "kotlin/src/Main.kt"))
            scopeToTouched(matched, changed).map { it.path } shouldBe listOf("AGENTS.md", "kotlin/AGENTS.md")
        }
    }

    Given("the cache key") {
        Then("it changes when a matched blob SHA changes") {
            val a = standardsCacheKey("o", "r", listOf(blob("AGENTS.md", "sha1")))
            val b = standardsCacheKey("o", "r", listOf(blob("AGENTS.md", "sha2")))
            (a == b) shouldBe false
            standardsCacheKey("o", "r", listOf(blob("AGENTS.md", "sha1"))) shouldBe a
        }
    }

    Given("parseRules") {
        Then("it recovers a rule array from fenced prose and dedups ids") {
            val rules = parseRules("```json\n$RULE_JSON\n```")
            rules.size shouldBe 1
            rules[0].id shouldBe "R1"
            rules[0].dimension shouldBe Dimension.PATTERN_VIOLATION
        }
        Then("a garbled or empty distillation degrades to no rules") {
            parseRules("not json") shouldBe emptyList()
            parseRules("[]") shouldBe emptyList()
            parseRules("[{\"id\":\"\",\"summary\":\"x\"}]") shouldBe emptyList()
        }
    }

    Given("Standards + the get_rule tool") {
        val std = buildStandards(
            listOf(Rule("R1", Dimension.PATTERN_VIOLATION, "wrap errors", "AGENTS.md")),
            mapOf("AGENTS.md" to "the full standards text"),
            listOf("AGENTS.md"),
        )
        Then("the menu, validId, ruleDoc, and sourceList behave") {
            isEmpty(std) shouldBe false
            val s = std.shouldNotBeNull()
            s.menu() shouldContain "- R1 [pattern_violation] wrap errors (source: AGENTS.md)"
            s.validId("R1") shouldBe true
            s.validId("R9") shouldBe false
            s.ruleDoc("R1") shouldBe "the full standards text"
            s.ruleDoc("R9") shouldBe ""
            s.sourceList() shouldBe listOf("AGENTS.md")
        }
        Then("standardsTools serves the full text and self-wraps a lookup") {
            val tool = standardsTools(std).single()
            tool.declaration.name shouldBe "get_rule"
            tool.execute(mapOf("id" to "R1")) shouldBe mapOf("rule" to "the full standards text")
            tool.execute(mapOf("id" to "R9")) shouldBe mapOf("rule" to "")
            standardsTools(null) shouldBe emptyList()
        }
        Then("writeStandardsMenu injects the menu only when standards are present") {
            val withStd = StringBuilder("P").also { writeStandardsMenu(it, std) }.toString()
            withStd shouldContain "Repo standards (cite rule_id for conformance findings)"
            StringBuilder("P").also { writeStandardsMenu(it, null) }.toString() shouldBe "P"
        }
        Then("buildDistillerInstruction fences each doc under a Document label") {
            val instr = buildDistillerInstruction("PROMPT", mapOf("AGENTS.md" to "body ``` more"), listOf("AGENTS.md"))
            instr shouldContain "### Document: AGENTS.md"
            instr shouldContain "````" // fence sized past the embedded triple-backtick run
        }
    }

    Given("gateCitations") {
        val std = buildStandards(listOf(Rule("R1", Dimension.PATTERN_VIOLATION, "s", "AGENTS.md")), mapOf("AGENTS.md" to "t"), listOf("AGENTS.md"))
        val uncited = Finding(dimension = Dimension.PATTERN_VIOLATION, severity = Severity.MAJOR, message = "unanchored")
        val cited = Finding(dimension = Dimension.PATTERN_VIOLATION, severity = Severity.MAJOR, message = "anchored", ruleId = "R1")
        val security = Finding(dimension = Dimension.SECURITY, severity = Severity.MAJOR, message = "sec")
        Then("uncited-drop removes an unanchored conformance finding, keeps cited + non-conformance") {
            val kept = gateCitations(stdEngine(uncitedDrop = true), listOf(uncited, cited, security), std)
            kept.map { it.message } shouldBe listOf("anchored", "sec")
        }
        Then("uncited-nitpick demotes rather than drops") {
            val kept = gateCitations(stdEngine(uncitedDrop = false), listOf(uncited), std)
            kept.single().severity shouldBe Severity.NITPICK
        }
        Then("standards-off passes findings through untouched") {
            gateCitations(stdEngine(enabled = false), listOf(uncited), std) shouldBe listOf(uncited)
        }
    }

    Given("the standards cache") {
        Then("a miss then a stored (even null) value hits") {
            val c = StandardsCache()
            c.get("k").ok shouldBe false
            c.put("k", null)
            c.get("k").let { it.ok shouldBe true; it.std shouldBe null }
        }
    }

    Given("discoverStandards end-to-end") {
        val gh = StdGh(Tree(listOf(blob("AGENTS.md")), truncated = false), mapOf("AGENTS.md" to "docs"))
        Then("it discovers, distills, caches, and returns the rule set") {
            val e = stdEngine(gh = gh)
            val std = discoverStandards(e, "acme", "api", "sha", listOf(PRFile(path = "src/Main.kt"))).shouldNotBeNull()
            std.rules.single().id shouldBe "R1"
            // Second call is served from the cache (same key).
            discoverStandards(e, "acme", "api", "sha", listOf(PRFile(path = "src/Main.kt"))).shouldNotBeNull().rules.size shouldBe 1
        }
        Then("it degrades to generic when disabled, truncated, or nothing matches") {
            discoverStandards(stdEngine(gh = gh, enabled = false), "a", "b", "s", emptyList()) shouldBe null
            val trunc = StdGh(Tree(listOf(blob("AGENTS.md")), truncated = true))
            discoverStandards(stdEngine(gh = trunc), "a", "b", "s", emptyList()) shouldBe null
            val none = StdGh(Tree(listOf(blob("src/Main.kt")), truncated = false))
            discoverStandards(stdEngine(gh = none), "a", "b", "s", emptyList()) shouldBe null
        }
        Then("a distillation that yields no rules degrades to generic (uncached null retained)") {
            val e = stdEngine(gh = gh, model = StdModel("[]"))
            discoverStandards(e, "acme", "api", "sha", listOf(PRFile(path = "src/Main.kt"))) shouldBe null
        }
    }
})
