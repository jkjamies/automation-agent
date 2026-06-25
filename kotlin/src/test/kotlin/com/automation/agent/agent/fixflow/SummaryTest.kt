package com.automation.agent.agent.fixflow

import com.automation.agent.githubapi.ChangedFile
import com.automation.agent.githubapi.Comparison
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import io.kotest.matchers.string.shouldNotContain
import kotlin.time.Duration.Companion.hours
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds

private fun changed(commits: Int, vararg files: String): Comparison =
    Comparison(totalCommits = commits, files = files.map { ChangedFile(path = it) })

class SummaryTest : BehaviorSpec({
    Given("a successful outcome") {
        When("building the summary") {
            Then("it frames success with attempts, changes, and the targeted findings") {
                val text = buildSummaryText(
                    SummaryInput(
                        outcome = TerminalOutcome.SUCCESS, workflow = "lint", fullRepo = "acme/api", prNumber = 7,
                        attempts = 2, report = "fix the unused import", changed = changed(1, "a.kt"),
                    ),
                )
                text shouldContain "acme/api: the lint fix passed CI after 2 attempts."
                text shouldContain "1 commit changed a.kt."
                text shouldContain "Targeted: fix the unused import"
            }
        }
    }

    Given("an exhausted outcome") {
        When("building the summary") {
            Then("it asks for review and inlines the remaining findings") {
                val text = buildSummaryText(
                    SummaryInput(
                        outcome = TerminalOutcome.EXHAUSTED, workflow = "coverage", fullRepo = "acme/api", prNumber = 7,
                        attempts = 3, lastOutput = "still 40% covered", changed = changed(3, "x.kt", "y.kt"),
                    ),
                )
                text shouldContain "still fails CI after 3 attempts. Please review."
                text shouldContain "3 commits changed x.kt, y.kt."
                text shouldContain "Remaining: still 40% covered"
            }
        }
    }

    Given("a timeout outcome") {
        When("building the summary") {
            Then("it reports the timeout, the awaited check, and 1-attempt phrasing") {
                val text = buildSummaryText(
                    SummaryInput(
                        outcome = TerminalOutcome.TIMEOUT, workflow = "lint", fullRepo = "acme/api", prNumber = 7,
                        attempts = 1, report = "tidy imports", timeout = "90m", checkName = "agent-lint-verify",
                    ),
                )
                text shouldContain "saw no CI result after 90m waiting for agent-lint-verify (1 attempt)."
                text shouldContain "No changes were recorded on the PR."
                text shouldContain "Targeted: tidy imports"
            }
        }
    }

    Given("more than eight changed files") {
        When("building the summary") {
            Then("the file list is truncated with a +N more suffix") {
                val files = (1..10).map { "f$it.kt" }.toTypedArray()
                val text = buildSummaryText(
                    SummaryInput(outcome = TerminalOutcome.SUCCESS, workflow = "lint", fullRepo = "acme/api", prNumber = 1, attempts = 1, changed = changed(1, *files)),
                )
                text shouldContain "(+2 more)"
                text shouldContain "f8.kt"
                text shouldNotContain "f9.kt"
            }
        }
    }

    Given("an empty findings blob") {
        When("building the summary") {
            Then("no findings line is appended") {
                val text = buildSummaryText(SummaryInput(outcome = TerminalOutcome.SUCCESS, workflow = "lint", fullRepo = "acme/api", prNumber = 1, attempts = 1, report = "   "))
                text shouldNotContain "Targeted:"
            }
        }
    }

    Given("various durations") {
        When("formatting the timeout") {
            Then("it uses the most compact whole unit") {
                formatTimeout(90.minutes) shouldBe "90m"
                formatTimeout(1.hours) shouldBe "1h"
                formatTimeout(30.seconds) shouldBe "30s"
            }
        }
    }
})
