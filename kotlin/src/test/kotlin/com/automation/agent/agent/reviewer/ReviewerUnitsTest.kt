package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.PRFile
import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.Kind
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import java.time.Instant
import kotlin.time.Duration.Companion.seconds

private fun file(path: String, patch: String = "", status: String = "modified", add: Int = 0, del: Int = 0) =
    PRFile(path = path, status = status, additions = add, deletions = del, patch = patch)

class ReviewerUnitsTest : BehaviorSpec({
    Given("the defensive findings parser") {
        When("the model wraps a JSON array in prose and fences") {
            Then("it recovers the findings") {
                val raw = "Here you go:\n```json\n[{\"file\":\"a.kt\",\"line\":3,\"dimension\":\"security\"," +
                    "\"severity\":\"critical\",\"message\":\"secret\",\"confidence\":0.9}]\n```\ndone"
                val found = parseFindings(raw)
                found.size shouldBe 1
                found[0].file shouldBe "a.kt"
                found[0].line shouldBe 3
                found[0].dimension shouldBe Dimension.SECURITY
                found[0].severity shouldBe Severity.CRITICAL
                found[0].confidence shouldBe 0.9
            }
        }
        When("an earlier non-findings array precedes the real one") {
            Then("it skips to the first decodable findings array") {
                val found = parseFindings("[1,2,3] then [{\"message\":\"hi\"}]")
                found.size shouldBe 1
                found[0].message shouldBe "hi"
            }
        }
        When("the body is malformed or empty") {
            Then("it degrades to no findings") {
                parseFindings("not json at all") shouldBe emptyList()
                parseFindings("[]") shouldBe emptyList()
                parseFindings("[{\"message\":\"\"}]") shouldBe emptyList() // empty message is unusable
            }
        }
        When("a confidence is non-finite") {
            Then("the array is rejected at validation (no findings)") {
                parseFindings("[{\"message\":\"x\",\"confidence\":1e999}]") shouldBe emptyList()
            }
        }
        When("a known string field has the wrong type") {
            Then("the array is rejected") {
                parseFindings("[{\"message\":\"x\",\"file\":123}]") shouldBe emptyList()
            }
        }
    }

    Given("severity and dimension normalization") {
        Then("unknown or blank values fall to the safe buckets") {
            normalizeSeverity("CRITICAL") shouldBe Severity.CRITICAL
            normalizeSeverity("bogus") shouldBe Severity.NITPICK
            normalizeSeverity("") shouldBe Severity.NITPICK
            normalizeDimension("Runtime-Safety") shouldBe Dimension.RUNTIME_SAFETY
            normalizeDimension("weird space") shouldBe Dimension.OTHER
            severityRank(Severity.CRITICAL) shouldBe 4
        }
    }

    Given("the fingerprint") {
        Then("it folds whitespace/case and omits the dimension") {
            val a = Finding(file = "a.kt", line = 5, dimension = Dimension.SECURITY, message = "Same   Message")
            val b = Finding(file = "a.kt", line = 5, dimension = Dimension.PERFORMANCE, message = "same message")
            fingerprint(a) shouldBe fingerprint(b)
        }
    }

    Given("the confidence clamps") {
        Then("the threshold folds NaN/negative to 0 and caps at 1") {
            clampThreshold(Double.NaN) shouldBe 0.0
            clampThreshold(-1.0) shouldBe 0.0
            clampThreshold(2.0) shouldBe 1.0
            clampThreshold(0.6) shouldBe 0.6
        }
        Then("the value clamp treats 0/absent as 0.5 and caps at 1") {
            clampConfidence(0.0) shouldBe 0.5
            clampConfidence(2.0) shouldBe 1.0
            clampConfidence(0.7) shouldBe 0.7
        }
    }

    Given("the two-dimensional size gate") {
        Then("it denies over the file cap or the byte cap and passes otherwise") {
            oversize(60, 10, 50, 262144).denied shouldBe true
            oversize(5, 300000, 50, 262144).denied shouldBe true
            oversize(5, 100, 50, 262144).denied shouldBe false
            oversize(9999, 9999999, 0, 0).denied shouldBe false // non-positive caps disable a dimension
        }
    }

    Given("the exclude-glob filter") {
        val filter = FileFilter(listOf("*.min.js", "vendor/**", "go.sum", ""))
        Then("basename globs, path globs, and ** all match") {
            filter.excluded("app.min.js") shouldBe true
            filter.excluded("a/b/app.min.js") shouldBe true
            filter.excluded("vendor/x/y.go") shouldBe true
            filter.excluded("go.sum") shouldBe true
            filter.excluded("src/main.kt") shouldBe false
        }
        Then("apply keeps non-excluded files and totals the patch bytes on the filtered set") {
            val result = filter.apply(listOf(file("app.min.js", "x".repeat(9999)), file("a.kt", "abc")))
            result.kept.map { it.path } shouldBe listOf("a.kt")
            result.diffBytes shouldBe 3
        }
    }

    Given("patch-byte accounting") {
        Then("it uses the patch length, estimates an omitted text diff, and charges binary as zero") {
            patchBytes(file("a.kt", "hello")) shouldBe 5
            patchBytes(file("big.kt", "", add = 3, del = 1)) shouldBe 4 * AVG_DIFF_LINE_BYTES
            patchBytes(file("img.png", "")) shouldBe 0
        }
    }

    Given("category selection") {
        Then("accessibility is gated to a UI/markup change") {
            selectCategories(listOf(file("a.kt"))).map { it.name } shouldBe
                listOf("safety", "security", "performance", "code_quality", "other")
            selectCategories(listOf(file("a.kt"), file("page.tsx"))).any { it.name == "accessibility" } shouldBe true
            hasUiFiles(listOf(file("styles.css"))) shouldBe true
        }
    }

    Given("the count-based scorecard") {
        Then("a critical in a critical dimension caps the overall to red") {
            val card = scoreFindings(
                listOf(
                    Finding(dimension = Dimension.SECURITY, severity = Severity.CRITICAL, message = "s"),
                    Finding(dimension = Dimension.READABILITY, severity = Severity.MEDIUM, message = "r"),
                ),
            )
            card.overall shouldBe Level.RED
            card.total shouldBe 2
        }
        Then("dimension thresholds and glyphs are as documented") {
            dimLevel(DimScore(Dimension.PERFORMANCE, major = 2)) shouldBe Level.RED
            dimLevel(DimScore(Dimension.PERFORMANCE, major = 1)) shouldBe Level.YELLOW
            dimLevel(DimScore(Dimension.PERFORMANCE, medium = 3)) shouldBe Level.YELLOW
            dimLevel(DimScore(Dimension.PERFORMANCE)) shouldBe Level.GREEN
            levelWord(Level.RED) shouldBe "Red"
            levelGlyph(Level.GREEN).isNotEmpty() shouldBe true
        }
    }

    Given("the deterministic glue gates") {
        Then("dropLowConfidence, dedupe, and demoteToNitpick behave") {
            val low = Finding(file = "a", line = 1, message = "m", confidence = 0.2)
            val high = Finding(file = "a", line = 1, message = "m", severity = Severity.MAJOR, confidence = 0.9)
            dropLowConfidence(listOf(low, high), 0.6).map { it.confidence } shouldBe listOf(0.9)
            // Same fingerprint, worst severity kept.
            val crit = Finding(file = "a", line = 1, message = "m", severity = Severity.CRITICAL, confidence = 0.5)
            dedupe(listOf(high, crit)).let {
                it.size shouldBe 1
                it[0].severity shouldBe Severity.CRITICAL
            }
            demoteToNitpick(listOf(crit)).all { it.severity == Severity.NITPICK } shouldBe true
        }
    }

    Given("diff formatting") {
        Then("it fences each file's patch and notes an omitted diff") {
            val diff = formatDiff(listOf(file("a.kt", "line", status = "modified"), file("img.png", "", status = "added")))
            diff shouldContain "### a.kt (modified)"
            diff shouldContain "```diff"
            diff shouldContain "(no textual diff available)"
        }
        Then("a patch with backtick runs gets a longer fence") {
            maxBacktickRun("a ``` b ```` c") shouldBe 4
            formatDiff(listOf(file("md.md", "text ``` more"))) shouldContain "````diff"
        }
    }

    Given("the intake helpers") {
        Then("splitFullName and isDependencyBot behave") {
            splitFullName("acme/api").let { it.ok shouldBe true; it.owner shouldBe "acme"; it.repo shouldBe "api" }
            splitFullName("nope").ok shouldBe false
            splitFullName("a/b/c").ok shouldBe false
            isDependencyBot("dependabot[bot]") shouldBe true
            isDependencyBot("renovate[bot]") shouldBe true
            isDependencyBot("alice") shouldBe false
        }
    }

    Given("instruction composition") {
        Then("the review and glue instructions embed the diff (and prior findings)") {
            buildReviewInstruction("PROMPT", "DIFF", null) shouldContain "## Diff under review"
            val glue = buildGlueInstruction("PROMPT", "DIFF", listOf(Finding(file = "a", line = 1, message = "m")), null)
            glue shouldContain "## Findings already reported by other lenses"
            glue shouldContain "\"file\":\"a\""
        }
    }

    Given("the debounce/coalesce hints") {
        val payload = "{\"action\":\"synchronize\",\"number\":42,\"repository\":{\"full_name\":\"acme/api\"}," +
            "\"pull_request\":{\"number\":42}}"
        Then("a synchronize review yields a byte-identical per-PR-per-window dedup name") {
            val e = Envelope.new(Kind.REVIEW, "webhook:/github", payload.toByteArray(), Instant.ofEpochSecond(1_000_000_000))
            val opts = enqueueOptions(e, 30.seconds)
            opts.name shouldBe "review-YWNtZS9hcGk-42-999999990000000000"
            opts.delay shouldBe 30.seconds
        }
        Then("a non-review kind, a non-synchronize action, or a non-positive debounce yields no hints") {
            val lint = Envelope.new(Kind.LINT, "webhook:/lint", payload.toByteArray(), Instant.EPOCH)
            enqueueOptions(lint, 30.seconds).name shouldBe null
            val opened = "{\"action\":\"opened\",\"number\":1,\"repository\":{\"full_name\":\"a/b\"},\"pull_request\":{\"number\":1}}"
            val e = Envelope.new(Kind.REVIEW, "webhook:/github", opened.toByteArray(), Instant.EPOCH)
            enqueueOptions(e, 30.seconds).name shouldBe null
            val sync = Envelope.new(Kind.REVIEW, "webhook:/github", payload.toByteArray(), Instant.EPOCH)
            enqueueOptions(sync, kotlin.time.Duration.ZERO).name shouldBe null
        }
    }

    Given("findingsJson") {
        Then("it renders the wire keys") {
            val js = findingsJson(listOf(Finding(file = "a.kt", line = 2, dimension = Dimension.SECURITY, message = "m", confidence = 0.5)))
            js shouldContain "\"dimension\":\"security\""
            js shouldContain "\"fix_prompt\":\"\""
        }
    }
})
