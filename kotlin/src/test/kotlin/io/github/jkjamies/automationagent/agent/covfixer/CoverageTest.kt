package io.github.jkjamies.automationagent.agent.covfixer

import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import io.github.jkjamies.automationagent.agent.fixflow.AnalyzeInput
import io.github.jkjamies.automationagent.agent.fixflow.Deps
import io.github.jkjamies.automationagent.agent.fixflow.FileWork
import io.github.jkjamies.automationagent.agent.fixflow.GitHub
import io.github.jkjamies.automationagent.agent.setup.assistantText
import io.github.jkjamies.automationagent.agent.setup.contentText
import io.github.jkjamies.automationagent.githubapi.Comparison
import io.github.jkjamies.automationagent.githubapi.Pr
import io.github.jkjamies.automationagent.githubapi.PrInput
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import java.io.File
import java.nio.file.Files

// Routes its response by the system prompt: triage, explore (plan), or execute (test).
private class ScriptedModel(val triage: String = "", val plan: String = "", val test: String = "") : Model {
    override val name: String = "scripted"
    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> = flow {
        val sys = contentText(request.config.systemInstruction)
        val resp =
            when {
                sys.contains("triaging") -> triage
                sys.contains("planning where to add") -> plan
                else -> test
            }
        emit(LlmResponse(content = assistantText(resp)))
    }
}

private val noopGitHub = object : GitHub {
    override suspend fun findOpenPrByBranch(owner: String, repo: String, branch: String): Pr? = null
    override suspend fun createPr(owner: String, repo: String, input: PrInput): Pr = throw NotImplementedError()
    override suspend fun addLabels(owner: String, repo: String, number: Int, labels: List<String>) {}
    override suspend fun compare(owner: String, repo: String, base: String, head: String): Comparison = Comparison()
}

class CoverageTest : BehaviorSpec({
    Given("a coverage triage array") {
        When("parsing it") {
            Then("non-empty entries with their uncovered items are kept") {
                val work = parseTriage("""[{"path":"calc.go","uncovered":["Divide error path","Add edge cases"]},{"path":"","uncovered":[]}]""")
                work shouldHaveSize 1
                work[0].path shouldBe "calc.go"
                work[0].items shouldHaveSize 2
            }
        }
    }

    Given("a scripted LLM") {
        When("triaging") {
            Then("it returns work, and an empty array errors") {
                val work = triage(ScriptedModel(triage = """[{"path":"calc.go","uncovered":["Divide"]}]"""), "jacoco xml")
                work shouldHaveSize 1
                work[0].path shouldBe "calc.go"
                shouldThrow<IllegalArgumentException> { triage(ScriptedModel(triage = "[]"), "report") }
            }
        }
    }

    Given("a plan array in noisy output") {
        When("parsing it") {
            Then("entries are keyed by source") {
                val plan = parsePlan("""prose [{"source":"calc.go","test_path":"calc_test.go","framework":"go testing","notes":"package calc"},{"source":"","test_path":"x"}] more""")
                plan.size shouldBe 1
                plan.getValue("calc.go").testPath shouldBe "calc_test.go"
                plan.getValue("calc.go").framework shouldBe "go testing"
            }
        }
    }

    Given("a checkout with a source file and an existing test") {
        When("analyzing (explore then execute)") {
            Then("a test is generated at the planned path") {
                val dir = Files.createTempDirectory("cov").toFile()
                File(dir, "calc.go").writeText("package calc\nfun divide(a: Int, b: Int) = a / b")
                File(dir, "ExistingTest.kt").writeText("class ExistingTest")
                val llm =
                    ScriptedModel(
                        plan = """[{"source":"calc.go","test_path":"calc_test.go","framework":"go testing","notes":"package calc"}]""",
                        test = "package calc\n\nfun testDivide() {}\n",
                    )
                val edits = analyze(AnalyzeInput(llm = llm, codeLlm = null, repoDir = dir.path, work = listOf(FileWork("calc.go", listOf("Divide"))), feedback = ""))
                edits shouldHaveSize 1
                edits[0].path shouldBe "calc_test.go"
                edits[0].content shouldContain "testDivide"
            }
        }
    }

    Given("an execute input") {
        When("building it") {
            Then("it includes the path, framework, notes, uncovered items, source and CI feedback") {
                val got = buildExecuteInput(FileWork("calc.go", listOf("Divide")), "package calc", PlanEntry(testPath = "calc_test.go", framework = "go testing", notes = "pkg calc"), "ci failed")
                for (want in listOf("calc_test.go", "go testing", "pkg calc", "Divide", "package calc", "ci failed")) got shouldContain want
            }
        }
    }

    Given("the coverage engine") {
        When("inspecting its identity") {
            Then("it carries the coverage check name and the common agent label") {
                val e = newEngine(Deps(gh = noopGitHub))
                e.checkName() shouldBe "agent-coverage-verify"
                e.label() shouldBe "automation-agent"
            }
        }
    }
})
