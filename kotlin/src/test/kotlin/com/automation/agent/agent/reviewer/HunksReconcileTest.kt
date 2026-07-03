package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.PRFile
import com.automation.agent.githubapi.ReviewCommentRef
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe

// A small unified-diff patch: hunk starting at new line 10, one context + two added + one removed.
private const val PATCH =
    "@@ -10,3 +10,4 @@\n" +
        " context\n" +
        "+added one\n" +
        "-removed\n" +
        "+added two\n"

class HunksReconcileTest : BehaviorSpec({
    Given("commentableLines") {
        Then("it returns the added and context head-side lines, skipping removed lines") {
            // new line 10 = context, 11 = added one, 12 = added two (removed advances old side only)
            commentableLines(PATCH) shouldBe setOf(10, 11, 12)
        }
        Then("a malformed or empty patch yields an empty set") {
            commentableLines("") shouldBe emptySet()
            commentableLines("no hunk header here") shouldBe emptySet()
            commentableLines("@@ nonsense @@\n+x") shouldBe emptySet()
        }
        Then("the '\\ No newline' marker and a body-ending blank line are handled") {
            val p = "@@ -1,1 +1,2 @@\n+one\n\\ No newline at end of file\n"
            commentableLines(p) shouldBe setOf(1)
        }
    }

    Given("DiffIndex") {
        val idx = DiffIndex(listOf(PRFile(path = "a.kt", patch = PATCH), PRFile(path = "b.kt", patch = "")))
        Then("inDiff reports commentable head-side lines per file") {
            idx.inDiff("a.kt", 11) shouldBe true
            idx.inDiff("a.kt", 99) shouldBe false
            idx.inDiff("b.kt", 1) shouldBe false
            idx.inDiff("unknown.kt", 1) shouldBe false
        }
    }

    Given("the fingerprint marker") {
        Then("fpMarker renders the exact contract and parseFpMarker round-trips") {
            fpMarker("a.kt:10:secret") shouldBe "<!-- ar-fp:a.kt:10:secret -->"
            parseFpMarker("body\n<!-- ar-fp:a.kt:10:secret -->\n") shouldBe "a.kt:10:secret"
            parseFpMarker("a foreign comment") shouldBe ""
        }
    }

    Given("reconcile") {
        Then("it posts new findings, keeps matched, and minimizes gone comments (sorted)") {
            val f = Finding(file = "a.kt", line = 10, message = "secret")
            val keep = ReviewCommentRef(nodeId = "n-keep", body = "x\n" + fpMarker(fingerprint(f)) + "\n")
            val gone = ReviewCommentRef(nodeId = "n-gone", body = "y\n" + fpMarker("a.kt:20:old") + "\n")
            val goneB = ReviewCommentRef(nodeId = "a-gone", body = "z\n" + fpMarker("a.kt:30:older") + "\n")
            val foreign = ReviewCommentRef(nodeId = "n-foreign", body = "no marker here")
            val g = Finding(file = "b.kt", line = 5, message = "new one")

            val res = reconcile(listOf(f, g), listOf(keep, gone, goneB, foreign))
            res.toPost.map { it.message } shouldBe listOf("new one") // f already has a comment; g is new
            res.toMinimize shouldBe listOf("a-gone", "n-gone") // sorted
        }
    }
})
