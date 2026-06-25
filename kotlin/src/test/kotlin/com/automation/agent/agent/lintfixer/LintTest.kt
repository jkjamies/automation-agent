package com.automation.agent.agent.lintfixer

import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import com.automation.agent.agent.fixflow.AnalyzeInput
import com.automation.agent.agent.fixflow.Deps
import com.automation.agent.agent.fixflow.FileWork
import com.automation.agent.agent.fixflow.GitHub
import com.automation.agent.agent.setup.assistantText
import com.automation.agent.githubapi.Comparison
import com.automation.agent.githubapi.Pr
import com.automation.agent.githubapi.PrInput
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import java.io.File
import java.nio.file.Files

private class StubModel(val text: String) : Model {
    override val name: String = "stub"
    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> = flow {
        emit(LlmResponse(content = assistantText(text)))
    }
}

private val noopGitHub = object : GitHub {
    override suspend fun findOpenPrByBranch(owner: String, repo: String, branch: String): Pr? = null
    override suspend fun createPr(owner: String, repo: String, input: PrInput): Pr = throw NotImplementedError()
    override suspend fun addLabels(owner: String, repo: String, number: Int, labels: List<String>) {}
    override suspend fun compare(owner: String, repo: String, base: String, head: String): Comparison = Comparison()
}

class LintTest : BehaviorSpec({
    Given("a triage JSON array in noisy output") {
        When("parsing it") {
            Then("non-empty entries are kept") {
                val work = parseTriage("""x [{"path":"a.go","problems":["unchecked error"]},{"path":"","problems":[]}] y""")
                work shouldHaveSize 1
                work[0].path shouldBe "a.go"
                work[0].items shouldHaveSize 1
            }
        }
    }

    Given("a stub LLM") {
        When("triaging") {
            Then("it returns work, and an empty array errors") {
                val work = triage(StubModel("""[{"path":"a.go","problems":["x"]}]"""), "report")
                work shouldHaveSize 1
                work[0].path shouldBe "a.go"
                shouldThrow<IllegalArgumentException> { triage(StubModel("[]"), "report") }
            }
        }
    }

    Given("a file work item") {
        When("building the per-file prompt") {
            Then("it includes the path, problems, source and CI feedback") {
                val p = buildFilePrompt(FileWork("a.go", listOf("unchecked error")), "package a", "ci failed")
                for (want in listOf("a.go", "unchecked error", "package a", "ci failed")) p shouldContain want
            }
        }
    }

    Given("a checkout with a source file") {
        When("analyzing it") {
            Then("the rewritten content is produced") {
                val dir = Files.createTempDirectory("lint").toFile()
                File(dir, "a.go").writeText("package a")
                val edits =
                    analyze(AnalyzeInput(llm = StubModel("package fixed\n"), codeLlm = null, repoDir = dir.path, work = listOf(FileWork("a.go", listOf("x"))), feedback = ""))
                edits shouldHaveSize 1
                edits[0].path shouldBe "a.go"
                edits[0].content shouldBe "package fixed\n"
            }
        }
    }

    Given("the lint engine") {
        When("inspecting its identity") {
            Then("it carries the lint check name and label") {
                val e = newEngine(Deps(gh = noopGitHub))
                e.checkName() shouldBe "agent-lint-verify"
                e.label() shouldBe "automation-agent"
            }
        }
    }
})
