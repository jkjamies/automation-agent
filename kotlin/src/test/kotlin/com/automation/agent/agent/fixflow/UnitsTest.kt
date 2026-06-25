package com.automation.agent.agent.fixflow

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain

class UnitsTest : BehaviorSpec({
    Given("kickoff envelopes") {
        When("parsing a valid one") {
            Then("it extracts owner/name, defaults base, and yields report text") {
                val k = parseKickoff("""{"repo":"acme/api","report":{"x":1}}""")
                k.owner() shouldBe "acme"
                k.name() shouldBe "api"
                k.base shouldBe "main"
                k.reportText() shouldBe """{"x":1}"""
            }
        }
        When("parsing invalid ones") {
            Then("each fails") {
                for (body in listOf("{", """{"report":{"x":1}}""", """{"repo":"noslash","report":{"x":1}}""", """{"repo":"a/b"}""")) {
                    shouldThrow<IllegalArgumentException> { parseKickoff(body) }
                }
            }
        }
    }

    Given("report shapes") {
        When("reading report text") {
            Then("a JSON value passes through and a JSON string is unquoted") {
                parseKickoff("""{"repo":"a/b","report":{"x":1}}""").reportText() shouldBe """{"x":1}"""
                parseKickoff("""{"repo":"a/b","report":"TN:\nSF:calc.go\nDA:7,0\n"}""").reportText() shouldBe "TN:\nSF:calc.go\nDA:7,0\n"
            }
        }
    }

    Given("model output") {
        When("extracting JSON and stripping fences") {
            Then("substrings and fenced content are recovered") {
                extractJsonArray("noise [1,2] x") shouldBe "[1,2]"
                extractJsonArray("none") shouldBe ""
                extractJsonObject("""x {"a":1} y""") shouldBe """{"a":1}"""
                extractJsonObject("none") shouldBe ""
                // Trailing prose with a stray bracket: the first complete value is returned.
                extractJsonArray("""[{"a":1}] then see [2]""") shouldBe """[{"a":1}]"""
                extractJsonObject("""{"a":1} note: closing }""") shouldBe """{"a":1}"""
                stripFences("```go\npackage x\n```") shouldBe "package x\n"
                stripFences("package y") shouldBe "package y\n"
            }
        }
    }

    Given("per-file work") {
        When("analyzing in parallel") {
            Then("non-empty edits are collected and sorted by original work path") {
                val edits =
                    parallelAnalyze(listOf(FileWork("b.go"), FileWork("a.go"))) { w ->
                        FileEdit(path = w.path + "_test.go", content = "package x\n")
                    }
                edits shouldHaveSize 2
                edits[0].path shouldBe "a.go_test.go"
                edits[1].path shouldBe "b.go_test.go"
            }
        }
        When("every analysis skips or there is no work") {
            Then("it reports no edits / no files") {
                shouldThrow<IllegalArgumentException> {
                    parallelAnalyze(listOf(FileWork("a.go"))) { FileEdit("", "") }
                }.message shouldContain "no edits"
                shouldThrow<IllegalArgumentException> { parallelAnalyze(emptyList()) { FileEdit("", "") } }
            }
        }
    }
})
